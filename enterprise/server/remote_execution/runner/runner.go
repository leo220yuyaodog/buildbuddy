package runner

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/buildbuddy-io/buildbuddy/enterprise/server/auth"
	"github.com/buildbuddy-io/buildbuddy/enterprise/server/remote_execution/commandutil"
	"github.com/buildbuddy-io/buildbuddy/enterprise/server/remote_execution/container"
	"github.com/buildbuddy-io/buildbuddy/enterprise/server/remote_execution/containers/bare"
	"github.com/buildbuddy-io/buildbuddy/enterprise/server/remote_execution/containers/docker"
	"github.com/buildbuddy-io/buildbuddy/enterprise/server/remote_execution/containers/firecracker"
	"github.com/buildbuddy-io/buildbuddy/enterprise/server/remote_execution/containers/podman"
	"github.com/buildbuddy-io/buildbuddy/enterprise/server/remote_execution/containers/sandbox"
	"github.com/buildbuddy-io/buildbuddy/enterprise/server/remote_execution/platform"
	"github.com/buildbuddy-io/buildbuddy/enterprise/server/remote_execution/vfs"
	"github.com/buildbuddy-io/buildbuddy/enterprise/server/remote_execution/workspace"
	"github.com/buildbuddy-io/buildbuddy/enterprise/server/tasksize"
	"github.com/buildbuddy-io/buildbuddy/enterprise/server/util/vfs_server"
	"github.com/buildbuddy-io/buildbuddy/server/environment"
	"github.com/buildbuddy-io/buildbuddy/server/interfaces"
	"github.com/buildbuddy-io/buildbuddy/server/metrics"
	"github.com/buildbuddy-io/buildbuddy/server/remote_cache/cachetools"
	"github.com/buildbuddy-io/buildbuddy/server/remote_cache/digest"
	"github.com/buildbuddy-io/buildbuddy/server/resources"
	"github.com/buildbuddy-io/buildbuddy/server/util/alert"
	"github.com/buildbuddy-io/buildbuddy/server/util/authutil"
	"github.com/buildbuddy-io/buildbuddy/server/util/background"
	"github.com/buildbuddy-io/buildbuddy/server/util/disk"
	"github.com/buildbuddy-io/buildbuddy/server/util/flagutil"
	"github.com/buildbuddy-io/buildbuddy/server/util/lockingbuffer"
	"github.com/buildbuddy-io/buildbuddy/server/util/log"
	"github.com/buildbuddy-io/buildbuddy/server/util/random"
	"github.com/buildbuddy-io/buildbuddy/server/util/status"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	fcpb "github.com/buildbuddy-io/buildbuddy/proto/firecracker"
	repb "github.com/buildbuddy-io/buildbuddy/proto/remote_execution"
	rspb "github.com/buildbuddy-io/buildbuddy/proto/resource"
	rnpb "github.com/buildbuddy-io/buildbuddy/proto/runner"
	scpb "github.com/buildbuddy-io/buildbuddy/proto/scheduler"
	vfspb "github.com/buildbuddy-io/buildbuddy/proto/vfs"
	wkpb "github.com/buildbuddy-io/buildbuddy/proto/worker"
	dockerclient "github.com/docker/docker/client"
)

var (
	rootDirectory           = flag.String("executor.root_directory", "/tmp/buildbuddy/remote_build", "The root directory to use for build files.")
	hostRootDirectory       = flag.String("executor.host_root_directory", "", "Path on the host where the executor container root directory is mounted.")
	dockerMountMode         = flag.String("executor.docker_mount_mode", "", "Sets the mount mode of volumes mounted to docker images. Useful if running on SELinux https://www.projectatomic.io/blog/2015/06/using-volumes-with-docker-can-cause-problems-with-selinux/")
	dockerNetHost           = flagutil.New("executor.docker_net_host", false, "Sets --net=host on the docker command. Intended for local development only.", flagutil.DeprecatedTag("Use --executor.docker_network=host instead."))
	dockerNetwork           = flag.String("executor.docker_network", "", "If set, set docker/podman --network to this value by default. Can be overridden per-action with the `dockerNetwork` exec property, which accepts values 'off' (--network=none) or 'bridge' (--network=<default>).")
	dockerCapAdd            = flag.String("docker_cap_add", "", "Sets --cap-add= on the docker command. Comma separated.")
	dockerSiblingContainers = flag.Bool("executor.docker_sibling_containers", false, "If set, mount the configured Docker socket to containers spawned for each action, to enable Docker-out-of-Docker (DooD). Takes effect only if docker_socket is also set. Should not be set by executors that can run untrusted code.")
	dockerDevices           = flagutil.New("executor.docker_devices", []container.DockerDeviceMapping{}, `Configure (docker) devices that will be available inside the sandbox container. Format is --executor.docker_devices='[{"PathOnHost":"/dev/foo","PathInContainer":"/some/dest","CgroupPermissions":"see,docker,docs"}]'`)
	dockerVolumes           = flagutil.New("executor.docker_volumes", []string{}, "Additional --volume arguments to be passed to docker or podman.")
	dockerInheritUserIDs    = flag.Bool("executor.docker_inherit_user_ids", false, "If set, run docker containers using the same uid and gid as the user running the executor process.")
	podmanRuntime           = flag.String("podman_runtime", "", "Enables running podman with other runtimes, like gVisor (runsc).")
	warmupTimeoutSecs       = flag.Int64("executor.warmup_timeout_secs", 120, "The default time (in seconds) to wait for an executor to warm up i.e. download the default docker image. Default is 120s")
	warmupWorkflowImages    = flag.Bool("executor.warmup_workflow_images", false, "Whether to warm up the Linux workflow images (firecracker only).")
	maxRunnerCount          = flag.Int("executor.runner_pool.max_runner_count", 0, "Maximum number of recycled RBE runners that can be pooled at once. Defaults to a value derived from estimated CPU usage, max RAM, allocated CPU, and allocated memory.")
	// How big a runner's workspace is allowed to get before we decide that it
	// can't be added to the pool and must be cleaned up instead.
	maxRunnerDiskSizeBytes = flag.Int64("executor.runner_pool.max_runner_disk_size_bytes", 16e9, "Maximum disk size for a recycled runner; runners exceeding this threshold are not recycled. Defaults to 16GB.")
	// How much memory a runner is allowed to use before we decide that it
	// can't be added to the pool and must be cleaned up instead.
	maxRunnerMemoryUsageBytes = flag.Int64("executor.runner_pool.max_runner_memory_usage_bytes", tasksize.WorkflowMemEstimate, "Maximum memory usage for a recycled runner; runners exceeding this threshold are not recycled. Defaults to 1/10 of total RAM allocated to the executor. (Only supported for Docker-based executors).")
	contextBasedShutdown      = flag.Bool("executor.context_based_shutdown_enabled", true, "Whether to remove runners using context cancelation. This is a transitional flag that will be removed in a future executor version.")
	podmanEnableStats         = flag.Bool("executor.podman.enable_stats", false, "Whether to enable cgroup-based podman stats.")
	podmanWarmupDefaultImages = flag.Bool("executor.podman.warmup_default_images", true, "Whether to warmup the default podman images or not.")
	bareEnableStats           = flag.Bool("executor.bare.enable_stats", false, "Whether to enable stats for bare command execution.")
)

const (
	// Runner states

	// initial means the container struct has been created but no actual container
	// has been created yet.
	initial state = iota
	// ready means the container is created and ready to run commands.
	ready
	// paused means the container is frozen and is eligible for addition to the
	// container pool.
	paused
	// removed means the container has been removed and cannot execute any more
	// commands.
	removed

	// How long to spend waiting for a runner to be removed before giving up.
	runnerCleanupTimeout = 30 * time.Second
	// Allowed time to spend trying to pause a runner and add it to the pool.
	runnerRecycleTimeout = 30 * time.Second
	// How long to spend waiting for a persistent worker process to terminate
	// after we send the shutdown signal before giving up.
	persistentWorkerShutdownTimeout = 10 * time.Second

	// Memory usage estimate multiplier for pooled runners, relative to the
	// default memory estimate for execution tasks.
	runnerMemUsageEstimateMultiplierBytes = 6.5

	// Label assigned to runner pool request count metric for fulfilled requests.
	hitStatusLabel = "hit"
	// Label assigned to runner pool request count metric for unfulfilled requests.
	missStatusLabel = "miss"

	// Value for persisent workers that support the JSON persistent worker protocol.
	workerProtocolJSONValue = "json"
	// Value for persisent workers that support the protobuf persistent worker protocol.
	workerProtocolProtobufValue = "proto"

	// Where to store the RunnerPoolState proto, relative to rootDirectory.
	stateFileName = "_runner_pool_state.bin"
)

var (
	podIDFromCpusetRegexp = regexp.MustCompile("/kubepods(/.*?)?/pod([a-z0-9\\-]{36})/")

	flagFilePattern           = regexp.MustCompile(`^(?:@|--?flagfile=)(.+)`)
	externalRepositoryPattern = regexp.MustCompile(`^@.*//.*`)
)

func k8sPodID() (string, error) {
	if _, err := os.Stat("/proc/1/cpuset"); err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	buf, err := os.ReadFile("/proc/1/cpuset")
	if err != nil {
		return "", err
	}
	cpuset := string(buf)
	if m := podIDFromCpusetRegexp.FindStringSubmatch(cpuset); m != nil {
		return m[2], nil
	}
	return "", nil
}

func ContextBasedShutdownEnabled() bool {
	return *contextBasedShutdown
}

// WarmupConfig specifies an image to be warmed up, for a specific isolation
// type.
type WarmupConfig struct {
	// Image is the image to be warmed up, NOT including the "docker://"
	// prefix.
	Image string

	// Isolation is the workload isolation type. An empty string corresponds
	// to the default isolation type.
	Isolation string
}

// state indicates the current state of a commandRunner.
type state int

func (s state) String() string {
	switch s {
	case initial:
		return "initial"
	case paused:
		return "paused"
	case ready:
		return "ready"
	case removed:
		return "removed"
	default:
		return "unknown"
	}
}

type runnerSlice []*commandRunner

func (rs runnerSlice) String() string {
	descriptions := make([]string, 0, len(rs))
	for _, r := range rs {
		descriptions = append(descriptions, r.String())
	}
	return "[" + strings.Join(descriptions, ", ") + "]"
}

type commandRunner struct {
	env            environment.Env
	imageCacheAuth *container.ImageCacheAuthenticator
	p              *pool

	// key controls which tasks can execute on this runner.
	key *rnpb.RunnerKey

	// PlatformProperties holds the parsed platform properties for the last task
	// executed by this runner.
	PlatformProperties *platform.Properties
	// debugID is a short debug ID used to identify this runner.
	// It is not necessarily globally unique.
	debugID string

	// Container is the handle on the container (possibly the bare /
	// NOP container) that is used to execute commands.
	Container *container.TracedCommandContainer
	// Workspace holds the data which is used by this runner.
	Workspace *workspace.Workspace
	// VFS holds the FUSE-backed virtual filesystem, if it's enabled.
	VFS *vfs.VFS
	// VFSServer holds the RPC server that serves FUSE filesystem requests.
	VFSServer *vfs_server.Server

	// task is the current task assigned to the runner.
	task *repb.ExecutionTask
	// taskNumber starts at 1 and is incremented each time the runner is
	// assigned a new task. Note: this is not necessarily the same as the number
	// of tasks that have actually been executed.
	taskNumber int64
	// State is the current state of the runner as it pertains to reuse.
	state state

	// TODO(bduffany): encapsulate persistent worker fields in their own struct
	// in a separate package.

	// Stdin handle to send persistent WorkRequests to.
	stdinWriter io.Writer
	// Stdout handle to read persistent WorkResponses from.
	// N.B. This is a bufio.Reader to support ByteReader required by ReadUvarint.
	stdoutReader *bufio.Reader
	stderr       lockingbuffer.LockingBuffer
	// Stops the persistent worker associated with this runner. If this is nil,
	// there is no persistent worker associated.
	stopPersistentWorker func() error
	// Keeps track of whether or not we encountered any errors that make the runner non-reusable.
	doNotReuse bool

	// Decoder used when reading streamed JSON values from stdout.
	jsonDecoder *json.Decoder

	// A function that is invoked after the runner is removed. Controlled by the
	// runner pool.
	removeCallback func()

	// Cached resource usage values from the last time the runner was added to
	// the pool.

	memoryUsageBytes int64
	diskUsageBytes   int64
}

func (r *commandRunner) String() string {
	ph, err := platformHash(r.key.Platform)
	if err != nil {
		ph = "<ERR!>"
	}
	return fmt.Sprintf(
		"%s:%s:%d:%s:%s:%s",
		r.debugID, r.state, r.taskNumber, r.key.GetGroupId(),
		truncate(r.key.InstanceName, 8, "..."), truncate(ph, 8, ""))
}

func (r *commandRunner) pullCredentials() (container.PullCredentials, error) {
	return container.GetPullCredentials(r.env, r.PlatformProperties)
}

func (r *commandRunner) PrepareForTask(ctx context.Context) error {
	r.Workspace.SetTask(ctx, r.task)
	// Clean outputs for the current task if applicable, in case
	// those paths were written as read-only inputs in a previous action.
	if r.PlatformProperties.RecycleRunner {
		if err := r.Workspace.Clean(); err != nil {
			log.CtxErrorf(ctx, "Failed to clean workspace: %s", err)
			return err
		}
	}
	if err := r.Workspace.CreateOutputDirs(); err != nil {
		return status.UnavailableErrorf("Error creating output directory: %s", err.Error())
	}

	// Pull the container image before Run() is called, so that we don't
	// use up the whole exec ctx timeout with a slow container pull.
	creds, err := r.pullCredentials()
	if err != nil {
		return err
	}
	err = container.PullImageIfNecessary(
		ctx, r.env, r.imageCacheAuth,
		r.Container, creds, r.PlatformProperties.ContainerImage,
	)
	if err != nil {
		return status.UnavailableErrorf("Error pulling container: %s", err)
	}

	return nil
}

func (r *commandRunner) DownloadInputs(ctx context.Context, ioStats *repb.IOStats) error {
	rootInstanceDigest := digest.NewResourceName(
		r.task.GetAction().GetInputRootDigest(),
		r.task.GetExecuteRequest().GetInstanceName(),
		rspb.CacheType_CAS, r.task.GetExecuteRequest().GetDigestFunction())
	inputTree, err := cachetools.GetTreeFromRootDirectoryDigest(ctx, r.env.GetContentAddressableStorageClient(), rootInstanceDigest)
	if err != nil {
		return err
	}

	layout := &container.FileSystemLayout{
		RemoteInstanceName: r.task.GetExecuteRequest().GetInstanceName(),
		DigestFunction:     r.task.GetExecuteRequest().GetDigestFunction(),
		Inputs:             inputTree,
		OutputDirs:         r.task.GetCommand().GetOutputDirectories(),
		OutputFiles:        r.task.GetCommand().GetOutputFiles(),
	}

	if r.PlatformProperties.EnableVFS {
		// Unlike other "container" implementations, for Firecracker VFS is mounted inside the guest VM so we need to
		// pass the layout information to the implementation.
		if fc, ok := r.Container.Delegate.(*firecracker.FirecrackerContainer); ok {
			fc.SetTaskFileSystemLayout(layout)
		}
	}

	if r.VFSServer != nil {
		p, err := vfs_server.NewCASLazyFileProvider(r.env, ctx, layout.RemoteInstanceName, layout.DigestFunction, layout.Inputs)
		if err != nil {
			return err
		}
		if err := r.VFSServer.Prepare(p); err != nil {
			return err
		}
	}
	if r.VFS != nil {
		if err := r.VFS.PrepareForTask(ctx, r.task.GetExecutionId()); err != nil {
			return err
		}
	}

	// Don't download inputs or add the CI runner if the FUSE-based filesystem is
	// enabled.
	// TODO(vadim): integrate VFS stats
	if r.VFS != nil {
		return nil
	}

	rxInfo, err := r.Workspace.DownloadInputs(ctx, inputTree)
	if err != nil {
		return err
	}
	if r.PlatformProperties.WorkflowID != "" {
		if err := r.Workspace.AddCIRunner(ctx); err != nil {
			return err
		}
	}
	ioStats.FileDownloadCount = rxInfo.FileCount
	ioStats.FileDownloadDurationUsec = rxInfo.TransferDuration.Microseconds()
	ioStats.FileDownloadSizeBytes = rxInfo.BytesTransferred
	return nil
}

// Run runs the task that is currently bound to the command runner.
func (r *commandRunner) Run(ctx context.Context) *interfaces.CommandResult {
	wsPath := r.Workspace.Path()
	if r.VFS != nil {
		wsPath = r.VFS.GetMountDir()
	}

	command := r.task.GetCommand()

	if !r.PlatformProperties.RecycleRunner {
		// If the container is not recyclable, then use `Run` to walk through
		// the entire container lifecycle in a single step.
		// TODO: Remove this `Run` method and call lifecycle methods directly.
		creds, err := r.pullCredentials()
		if err != nil {
			return commandutil.ErrorResult(err)
		}
		return r.Container.Run(ctx, command, wsPath, creds)
	}

	// Get the container to "ready" state so that we can exec commands in it.
	//
	// TODO(bduffany): Make this access to r.state thread-safe. The pool can be
	// shutdown while this func is executing, which concurrently sets the runner
	// state to "removed". This doesn't cause any known issues right now, but is
	// error prone.
	r.p.mu.RLock()
	s := r.state
	r.p.mu.RUnlock()
	switch s {
	case initial:
		creds, err := r.pullCredentials()
		if err != nil {
			return commandutil.ErrorResult(err)
		}
		err = container.PullImageIfNecessary(
			ctx, r.env, r.imageCacheAuth,
			r.Container, creds, r.PlatformProperties.ContainerImage,
		)
		if err != nil {
			return commandutil.ErrorResult(err)
		}
		if err := r.Container.Create(ctx, wsPath); err != nil {
			return commandutil.ErrorResult(err)
		}
		r.p.mu.Lock()
		r.state = ready
		r.p.mu.Unlock()
		break
	case ready:
		break
	case removed:
		return commandutil.ErrorResult(status.UnavailableErrorf("Not starting new task since executor is shutting down"))
	default:
		return commandutil.ErrorResult(status.InternalErrorf("unexpected runner state %d; this should never happen", r.state))
	}

	if r.supportsPersistentWorkers(ctx, command) {
		return r.sendPersistentWorkRequest(ctx, command)
	}

	return r.Container.Exec(ctx, command, &container.Stdio{})
}

func (r *commandRunner) UploadOutputs(ctx context.Context, ioStats *repb.IOStats, actionResult *repb.ActionResult, cmdResult *interfaces.CommandResult) error {
	txInfo, err := r.Workspace.UploadOutputs(ctx, r.task.Command, actionResult, cmdResult)
	if err != nil {
		return err
	}
	ioStats.FileUploadCount = txInfo.FileCount
	ioStats.FileUploadDurationUsec = txInfo.TransferDuration.Microseconds()
	ioStats.FileUploadSizeBytes = txInfo.BytesTransferred
	return nil
}

func (r *commandRunner) GetIsolationType() string {
	return r.PlatformProperties.WorkloadIsolationType
}

// shutdown runs any manual cleanup required to clean up processes before
// removing a runner from the pool. This has no effect for isolation types
// that fully isolate all processes started by the runner and remove them
// automatically via `Container.Remove`.
func (r *commandRunner) shutdown(ctx context.Context) error {
	r.p.mu.RLock()
	props := r.PlatformProperties
	r.p.mu.RUnlock()

	if props.WorkloadIsolationType != string(platform.BareContainerType) {
		return nil
	}

	if r.isCIRunner() {
		if err := r.cleanupCIRunner(ctx); err != nil {
			return err
		}
	}

	return nil
}

func (r *commandRunner) Remove(ctx context.Context) error {
	if r.removeCallback != nil {
		defer r.removeCallback()
	}

	r.p.mu.RLock()
	s := r.state
	r.p.mu.RUnlock()

	errs := []error{}
	if s != initial && s != removed {
		r.p.mu.Lock()
		r.state = removed
		r.p.mu.Unlock()
		if err := r.shutdown(ctx); err != nil {
			errs = append(errs, err)
		}
		if err := r.Container.Remove(ctx); err != nil {
			errs = append(errs, err)
		}
		if r.stopPersistentWorker != nil {
			if err := r.stopPersistentWorker(); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if r.VFS != nil {
		if err := r.VFS.Unmount(); err != nil {
			errs = append(errs, err)
		}
	}
	if r.VFSServer != nil {
		r.VFSServer.Stop()
	}
	if err := r.Workspace.Remove(ctx); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return errSlice(errs)
	}
	return nil
}

func (r *commandRunner) RemoveWithTimeout(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, runnerCleanupTimeout)
	defer cancel()
	return r.Remove(ctx)
}

func (r *commandRunner) RemoveInBackground() {
	// TODO: Add to a cleanup queue instead of spawning a goroutine here.
	go func() {
		if err := r.RemoveWithTimeout(context.Background()); err != nil {
			log.Errorf("Failed to remove runner: %s", err)
		}
	}()
}

// isCIRunner returns whether the task assigned to this runner is a BuildBuddy
// CI task.
func (r *commandRunner) isCIRunner() bool {
	r.p.mu.RLock()
	task := r.task
	props := r.PlatformProperties
	r.p.mu.RUnlock()

	args := task.GetCommand().GetArguments()
	return props.WorkflowID != "" && len(args) > 0 && args[0] == "./buildbuddy_ci_runner"
}

func (r *commandRunner) cleanupCIRunner(ctx context.Context) error {
	// Run the currently assigned buildbuddy_ci_runner command, appending the
	// --shutdown_and_exit argument. We use this approach because we want to
	// preserve the configuration from the last run command, which may include the
	// configured Bazel path.
	cleanupCmd := proto.Clone(r.task.GetCommand()).(*repb.Command)
	cleanupCmd.Arguments = append(cleanupCmd.Arguments, "--shutdown_and_exit")

	res := commandutil.Run(ctx, cleanupCmd, r.Workspace.Path(), nil /*=statsListener*/, &container.Stdio{})
	return res.Error
}

type ContainerProvider func(ctx context.Context, props *platform.Properties, st *repb.ScheduledTask, state *rnpb.RunnerState, workDir string) (*container.TracedCommandContainer, error)

type PoolOptions struct {
	// ContainerProvider is an optional implementation overriding
	// newContainerImpl.
	ContainerProvider ContainerProvider
}

type pool struct {
	env            environment.Env
	imageCacheAuth *container.ImageCacheAuthenticator
	podID          string
	buildRoot      string
	dockerClient   *dockerclient.Client
	podmanProvider *podman.Provider
	newContainer   ContainerProvider

	maxRunnerCount            int
	maxRunnerMemoryUsageBytes int64
	maxRunnerDiskUsageBytes   int64

	// pendingRemovals keeps track of which runners are pending removal.
	pendingRemovals sync.WaitGroup

	mu             sync.RWMutex // protects(isShuttingDown), protects(runners)
	isShuttingDown bool
	// runners holds all runners managed by the pool.
	runners []*commandRunner
}

func NewPool(env environment.Env, opts *PoolOptions) (*pool, error) {
	hc := env.GetHealthChecker()
	if hc == nil {
		return nil, status.FailedPreconditionError("Missing health checker")
	}
	podID, err := k8sPodID()
	if err != nil {
		return nil, status.FailedPreconditionErrorf("Failed to determine k8s pod ID: %s", err)
	}

	var dockerClient *dockerclient.Client
	if platform.DockerSocket() != "" {
		_, err := os.Stat(platform.DockerSocket())
		if os.IsNotExist(err) {
			return nil, status.FailedPreconditionErrorf("Docker socket %q not found", platform.DockerSocket())
		}
		dockerSocket := platform.DockerSocket()
		if !strings.Contains(dockerSocket, "://") {
			dockerSocket = fmt.Sprintf("unix://%s", dockerSocket)
		}
		dockerClient, err = dockerclient.NewClientWithOpts(
			dockerclient.WithHost(dockerSocket),
			dockerclient.WithAPIVersionNegotiation(),
		)
		if err != nil {
			return nil, status.FailedPreconditionErrorf("Failed to create docker client: %s", err)
		}
	}

	imageCacheAuth := container.NewImageCacheAuthenticator(container.ImageCacheAuthenticatorOpts{})

	podmanProvider, err := podman.NewProvider(env, imageCacheAuth, *rootDirectory)
	if err != nil {
		return nil, err
	}

	p := &pool{
		env:            env,
		podID:          podID,
		dockerClient:   dockerClient,
		podmanProvider: podmanProvider,
		buildRoot:      *rootDirectory,
		imageCacheAuth: imageCacheAuth,
		runners:        []*commandRunner{},
	}
	p.newContainer = p.newContainerImpl
	if opts.ContainerProvider != nil {
		p.newContainer = opts.ContainerProvider
	}
	p.setLimits()
	hc.RegisterShutdownFunction(p.Shutdown)

	if err := p.initializeFromSavedState(env.GetServerContext()); err != nil {
		log.Warningf("Failed to initialize runner pool from saved state: %s", err)
	}

	return p, nil
}

func (p *pool) GetBuildRoot() string {
	return p.buildRoot
}

// Add pauses the runner and makes it available to be returned from the pool
// via Get.
//
// If an error is returned, the runner was not successfully added to the pool,
// and should be removed.
func (p *pool) Add(ctx context.Context, r *commandRunner) error {
	if err := p.add(ctx, r); err != nil {
		metrics.RunnerPoolFailedRecycleAttempts.With(prometheus.Labels{
			metrics.RunnerPoolFailedRecycleReason: err.Label,
		}).Inc()
		return err.Error
	}
	return nil
}

func (p *pool) checkAddPreconditions(r *commandRunner) *labeledError {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if p.isShuttingDown {
		return &labeledError{
			status.UnavailableError("pool is shutting down; new runners cannot be added."),
			"pool_shutting_down",
		}
	}
	// Note: shutdown can change the state to removed, so we need the lock to be
	// held for this check.
	if r.state != ready {
		return &labeledError{
			status.InternalErrorf("unexpected runner state %d; this should never happen", r.state),
			"unexpected_runner_state",
		}
	}
	return nil
}

func (p *pool) add(ctx context.Context, r *commandRunner) *labeledError {
	if err := p.checkAddPreconditions(r); err != nil {
		return err
	}

	if err := r.Container.Pause(ctx); err != nil {
		return &labeledError{
			status.WrapError(err, "failed to pause container before adding to the pool"),
			"pause_failed",
		}
	}

	stats, err := r.Container.Stats(ctx)
	if err != nil {
		return &labeledError{
			status.WrapError(err, "failed to compute container stats"),
			"stats_failed",
		}
	}
	// If memory usage stats are not implemented, fall back to the default task
	// size estimate.
	if stats == nil {
		stats = &repb.UsageStats{}
		stats.MemoryBytes = int64(float64(tasksize.DefaultMemEstimate) * runnerMemUsageEstimateMultiplierBytes)
	}

	if stats.MemoryBytes > p.maxRunnerMemoryUsageBytes {
		return &labeledError{
			status.ResourceExhaustedErrorf("runner memory usage of %d bytes exceeds limit of %d bytes", stats.MemoryBytes, p.maxRunnerMemoryUsageBytes),
			"max_memory_exceeded",
		}
	}
	du, err := r.Workspace.DiskUsageBytes()
	if err != nil {
		return &labeledError{
			status.WrapError(err, "failed to compute runner disk usage"),
			"compute_disk_usage_failed",
		}
	}
	if du > p.maxRunnerDiskUsageBytes {
		return &labeledError{
			status.ResourceExhaustedErrorf("runner disk usage of %d bytes exceeds limit of %d bytes", du, p.maxRunnerDiskUsageBytes),
			"max_disk_usage_exceeded",
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	// The pool might have shut down while we were pausing the container. We don't
	// hold the lock while pausing since it is relatively slow, so need to re-check
	// whether the pool shut down here.
	if p.isShuttingDown {
		r.RemoveInBackground()
		return nil
	}

	if p.maxRunnerCount <= 0 {
		return &labeledError{
			status.InternalError("pool max runner count is <= 0; this should never happen"),
			"max_runner_count_zero",
		}
	}

	for p.pausedRunnerCount() >= p.maxRunnerCount {
		// Evict the oldest (first) paused runner to make room for the new one.
		evictIndex := -1
		for i, r := range p.runners {
			if r.state == paused {
				evictIndex = i
				break
			}
		}
		if evictIndex == -1 {
			return &labeledError{
				status.InternalError("could not find runner to evict; this should never happen"),
				"evict_failed",
			}
		}

		r := p.runners[evictIndex]
		if p.pausedRunnerCount() >= p.maxRunnerCount {
			log.Infof("Evicting runner %s (pool max count %d exceeded).", r, p.maxRunnerCount)
		} else if p.pausedRunnerMemoryUsageBytes()+stats.MemoryBytes > p.maxRunnerMemoryUsageBytes {
			log.Infof("Evicting runner %s (max memory %d exceeded).", r, p.maxRunnerMemoryUsageBytes)
		}
		p.runners = append(p.runners[:evictIndex], p.runners[evictIndex+1:]...)

		metrics.RunnerPoolEvictions.Inc()
		metrics.RunnerPoolCount.Dec()
		metrics.RunnerPoolDiskUsageBytes.Sub(float64(r.diskUsageBytes))
		metrics.RunnerPoolMemoryUsageBytes.Sub(float64(r.memoryUsageBytes))

		r.RemoveInBackground()
	}

	// Shift this runner to the end of the list since we want to keep the list
	// sorted in increasing order of `Add` timestamp (per our LRU eviction policy).
	p.remove(r)
	p.runners = append(p.runners, r)

	// Cache resource usage values so we don't need to recompute them when
	// updating metrics upon removal.
	r.memoryUsageBytes = stats.MemoryBytes
	r.diskUsageBytes = du

	metrics.RunnerPoolDiskUsageBytes.Add(float64(r.diskUsageBytes))
	metrics.RunnerPoolMemoryUsageBytes.Add(float64(r.memoryUsageBytes))
	metrics.RunnerPoolCount.Inc()

	// Officially mark this runner paused and ready for reuse.
	r.state = paused

	return nil
}

func (p *pool) hostBuildRoot() string {
	// If host root dir is explicitly configured, prefer that.
	if *hostRootDirectory != "" {
		return filepath.Join(*hostRootDirectory, "remotebuilds")
	}
	if p.podID == "" {
		// Probably running on bare metal -- return the build root directly.
		return p.buildRoot
	}
	// Running on k8s -- return the path to the build root on the *host* node.
	// TODO(bduffany): Make this configurable in YAML, populating {{.PodID}} via template.
	// People might have conventions other than executor-data for the volume name + remotebuilds
	// for the build root dir.
	return fmt.Sprintf("/var/lib/kubelet/pods/%s/volumes/kubernetes.io~empty-dir/executor-data/remotebuilds", p.podID)
}

func (p *pool) dockerOptions() *docker.DockerOptions {
	return &docker.DockerOptions{
		Socket:                  platform.DockerSocket(),
		EnableSiblingContainers: *dockerSiblingContainers,
		UseHostNetwork:          *dockerNetHost,
		DockerMountMode:         *dockerMountMode,
		DockerCapAdd:            *dockerCapAdd,
		DockerDevices:           *dockerDevices,
		DefaultNetworkMode:      *dockerNetwork,
		Volumes:                 *dockerVolumes,
		InheritUserIDs:          *dockerInheritUserIDs,
	}
}

func (p *pool) warmupImage(ctx context.Context, cfg *WarmupConfig) error {
	start := time.Now()
	log.Infof("Warming up %s image %q", cfg.Isolation, cfg.Image)
	plat := &repb.Platform{
		Properties: []*repb.Platform_Property{
			{Name: "container-image", Value: platform.DockerPrefix + cfg.Image},
			{Name: "workload-isolation-type", Value: cfg.Isolation},
		},
	}
	task := &repb.ExecutionTask{
		Command: &repb.Command{
			Arguments: []string{"echo", "'warmup'"},
			Platform:  plat,
		},
	}
	platProps := platform.ParseProperties(task)
	platform.ApplyOverrides(p.env, platform.GetExecutorProperties(), platProps, task.GetCommand())
	st := &repb.ScheduledTask{
		SchedulingMetadata: &scpb.SchedulingMetadata{
			// Note: this will use the default task size estimates and not
			// measurement-based task sizing, which requires the app.
			TaskSize: tasksize.Estimate(task),
		},
		ExecutionTask: task,
	}

	state := &rnpb.RunnerState{
		// Note: warmup runner is not tied to a group or instance name
		RunnerKey: &rnpb.RunnerKey{Platform: plat},
	}

	ws, err := workspace.New(p.env, p.GetBuildRoot(), &workspace.Opts{})
	defer func() {
		ctx, cancel := background.ExtendContextForFinalization(ctx, runnerCleanupTimeout)
		defer cancel()
		_ = ws.Remove(ctx)
	}()
	c, err := p.newContainer(ctx, platProps, st, state, ws.Path())
	if err != nil {
		log.Errorf("Error warming up %q image %q: %s", cfg.Isolation, cfg.Image, err)
		return err
	}

	creds, err := container.GetPullCredentials(p.env, platProps)
	if err != nil {
		return err
	}
	err = container.PullImageIfNecessary(
		ctx, p.env, p.imageCacheAuth,
		c, creds, platProps.ContainerImage,
	)
	if err != nil {
		return err
	}
	log.Infof("Warmup: %s pulled image %q in %s", cfg.Isolation, cfg.Image, time.Since(start))
	return nil
}

func (p *pool) Warmup(ctx context.Context) {
	start := time.Now()
	defer func() {
		log.Infof("Warmup: pulled all images in %s", time.Since(start))
	}()
	// Give the pull up to 2 minute to succeed.
	// In practice warmup take about 30 seconds for docker and 75 seconds for firecracker.
	timeout := 2 * time.Minute
	if *warmupTimeoutSecs > 0 {
		timeout = time.Duration(*warmupTimeoutSecs) * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	eg, ctx := errgroup.WithContext(ctx)
	for _, cfg := range p.warmupConfigs() {
		cfg := cfg
		eg.Go(func() error {
			return p.warmupImage(ctx, &cfg)
		})
	}
	if err := eg.Wait(); err != nil {
		log.Warningf("Error warming up containers: %s", err)
	}
}

func (p *pool) warmupConfigs() []WarmupConfig {
	var out []WarmupConfig
	for _, isolation := range platform.GetExecutorProperties().SupportedIsolationTypes {
		if isolation == platform.PodmanContainerType && !*podmanWarmupDefaultImages {
			continue
		}

		// Warm up the default execution image for all isolation types, as well
		// as the new Ubuntu 20.04 image.
		out = append(out, WarmupConfig{
			Image:     platform.DefaultImage(),
			Isolation: string(isolation),
		})
		out = append(out, WarmupConfig{
			Image:     platform.Ubuntu20_04Image,
			Isolation: string(isolation),
		})

		// If firecracker is supported, additionally warm up the workflow images.
		if *warmupWorkflowImages && isolation == platform.FirecrackerContainerType {
			out = append(out, WarmupConfig{
				Image:     platform.Ubuntu18_04WorkflowsImage,
				Isolation: string(isolation),
			})
			out = append(out, WarmupConfig{
				Image:     platform.Ubuntu20_04WorkflowsImage,
				Isolation: string(isolation),
			})
		}
	}
	return out
}

func (p *pool) effectivePlatform(task *repb.ExecutionTask) (*platform.Properties, error) {
	props := platform.ParseProperties(task)
	// TODO: This mutates the task; find a cleaner way to do this.
	if err := platform.ApplyOverrides(p.env, platform.GetExecutorProperties(), props, task.GetCommand()); err != nil {
		return nil, err
	}
	return props, nil
}

// Get returns a runner bound to the the given task. The caller must call
// TryRecycle on the returned runner when done using it.
//
// If the task has runner recycling enabled then it attempts to find a runner
// from the pool that can execute the task. If runner recycling is disabled or
// if there are no eligible paused runners, it creates and returns a new runner.
//
// The returned runner is considered "active" and will be killed if the
// executor is shut down.
func (p *pool) Get(ctx context.Context, st *repb.ScheduledTask) (interfaces.Runner, error) {
	task := st.ExecutionTask
	props, err := p.effectivePlatform(task)
	if err != nil {
		return nil, err
	}
	user, err := auth.UserFromTrustedJWT(ctx)
	if err != nil && !authutil.IsAnonymousUserError(err) {
		return nil, err
	}
	groupID := ""
	if user != nil {
		groupID = user.GetGroupID()
	}
	if props.RecycleRunner && err != nil {
		return nil, status.InvalidArgumentError(
			"runner recycling is not supported for anonymous builds " +
				`(recycling was requested via platform property "recycle-runner=true")`)
	}
	if props.RecycleRunner && props.EnableVFS {
		return nil, status.InvalidArgumentError("VFS is not yet supported for recycled runners")
	}

	key := &rnpb.RunnerKey{
		GroupId:             groupID,
		InstanceName:        task.GetExecuteRequest().GetInstanceName(),
		Platform:            task.GetCommand().GetPlatform(),
		PersistentWorkerKey: effectivePersistentWorkerKey(props, task.GetCommand().GetArguments()),
	}
	if props.RecycleRunner {
		r, err := p.take(ctx, key)
		if err != nil {
			return nil, err
		}
		if r != nil {
			p.mu.Lock()
			r.task = task
			r.taskNumber += 1
			r.PlatformProperties = props
			p.mu.Unlock()
			log.CtxInfof(ctx, "Reusing existing runner %s for task", r)
			return r, nil
		}
	}

	debugID, _ := random.RandomString(8)
	state := &rnpb.RunnerState{
		RunnerKey:         key,
		DebugId:           debugID,
		AssignedTaskCount: 1,
	}
	return p.newRunner(ctx, props, st, state)
}

// newRunner creates a runner either for the given task (if set) or restores the
// runner from the given state.ContainerState.
func (p *pool) newRunner(ctx context.Context, props *platform.Properties, st *repb.ScheduledTask, state *rnpb.RunnerState) (*commandRunner, error) {
	if st == nil && state.GetContainerState() == nil {
		return nil, status.FailedPreconditionError("either a task or saved container state is required to create a runner")
	}
	wsOpts := &workspace.Opts{
		Preserve:        props.PreserveWorkspace,
		CleanInputs:     props.CleanWorkspaceInputs,
		NonrootWritable: props.NonrootWorkspace || props.DockerUser != "",
	}
	ws, err := workspace.New(p.env, p.buildRoot, wsOpts)
	if err != nil {
		return nil, err
	}
	ctr, err := p.newContainer(ctx, props, st, state, ws.Path())
	if err != nil {
		return nil, err
	}
	var fs *vfs.VFS
	var vfsServer *vfs_server.Server
	enableVFS := props.EnableVFS
	// Firecracker requires mounting the FS inside the guest VM so we can't just swap out the directory in the runner.
	if enableVFS && platform.ContainerType(props.WorkloadIsolationType) != platform.FirecrackerContainerType {
		vfsDir := ws.Path() + "_vfs"
		if err := os.Mkdir(vfsDir, 0755); err != nil {
			return nil, status.UnavailableErrorf("could not create FUSE FS dir: %s", err)
		}

		vfsServer = vfs_server.New(p.env, ws.Path())
		unixSocket := filepath.Join(ws.Path(), "vfs.sock")

		lis, err := net.Listen("unix", unixSocket)
		if err != nil {
			return nil, err
		}
		if err := vfsServer.Start(lis); err != nil {
			return nil, err
		}

		conn, err := grpc.Dial("unix://"+unixSocket, grpc.WithInsecure())
		if err != nil {
			return nil, err
		}
		vfsClient := vfspb.NewFileSystemClient(conn)
		fs = vfs.New(vfsClient, vfsDir, &vfs.Options{})
		if err := fs.Mount(); err != nil {
			return nil, status.UnavailableErrorf("unable to mount VFS at %q: %s", vfsDir, err)
		}
	}
	r := &commandRunner{
		env:                p.env,
		p:                  p,
		imageCacheAuth:     p.imageCacheAuth,
		key:                state.GetRunnerKey(),
		debugID:            state.GetDebugId(),
		taskNumber:         state.GetAssignedTaskCount(),
		task:               st.GetExecutionTask(),
		PlatformProperties: props,
		Container:          ctr,
		Workspace:          ws,
		VFS:                fs,
		VFSServer:          vfsServer,
	}
	// If we restored a paused container from state, the initial state should be
	// set to "paused" rather than the usual "init".
	if state.GetContainerState() != nil {
		r.state = paused
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.isShuttingDown {
		return nil, status.UnavailableErrorf("Could not get a new task runner because the executor is shutting down.")
	}
	p.runners = append(p.runners, r)
	if *contextBasedShutdown {
		p.pendingRemovals.Add(1)
		r.removeCallback = func() {
			p.pendingRemovals.Done()
		}
	}
	log.CtxInfof(ctx, "Created new %s runner %s for task", props.WorkloadIsolationType, r)
	return r, nil
}

func (p *pool) newContainerImpl(ctx context.Context, props *platform.Properties, task *repb.ScheduledTask, state *rnpb.RunnerState, workingDir string) (*container.TracedCommandContainer, error) {
	if state.GetContainerState() != nil {
		if props.WorkloadIsolationType != string(platform.FirecrackerContainerType) {
			return nil, status.UnimplementedErrorf("restoring container state is not implemented for %q isolation", string(props.WorkloadIsolationType))
		}
	}

	var ctr container.CommandContainer
	switch platform.ContainerType(props.WorkloadIsolationType) {
	case platform.DockerContainerType:
		opts := p.dockerOptions()
		opts.ForceRoot = props.DockerForceRoot
		opts.DockerUser = props.DockerUser
		opts.DockerNetwork = props.DockerNetwork
		ctr = docker.NewDockerContainer(
			p.env, p.imageCacheAuth, p.dockerClient, props.ContainerImage,
			p.hostBuildRoot(), opts,
		)
	case platform.PodmanContainerType:
		opts := &podman.PodmanOptions{
			ForceRoot:            props.DockerForceRoot,
			User:                 props.DockerUser,
			Network:              props.DockerNetwork,
			DefaultNetworkMode:   *dockerNetwork,
			CapAdd:               *dockerCapAdd,
			Devices:              *dockerDevices,
			Volumes:              *dockerVolumes,
			Runtime:              *podmanRuntime,
			EnableStats:          *podmanEnableStats,
			EnableImageStreaming: props.EnablePodmanImageStreaming,
		}
		c, err := p.podmanProvider.NewContainer(ctx, props.ContainerImage, opts)
		if err != nil {
			return nil, err
		}
		ctr = c
	case platform.FirecrackerContainerType:
		var vmConfig *fcpb.VMConfiguration
		savedState := state.GetContainerState().GetFirecrackerState()
		if savedState == nil {
			sizeEstimate := task.GetSchedulingMetadata().GetTaskSize()
			vmConfig = &fcpb.VMConfiguration{
				NumCpus:           int64(math.Max(1.0, float64(sizeEstimate.GetEstimatedMilliCpu())/1000)),
				MemSizeMb:         int64(math.Max(1.0, float64(sizeEstimate.GetEstimatedMemoryBytes())/1e6)),
				ScratchDiskSizeMb: int64(float64(sizeEstimate.GetEstimatedFreeDiskBytes()) / 1e6),
				EnableNetworking:  true,
				InitDockerd:       props.InitDockerd,
			}
		} else {
			vmConfig = state.GetContainerState().GetFirecrackerState().GetVmConfiguration()
		}
		opts := firecracker.ContainerOpts{
			VMConfiguration:        vmConfig,
			SavedState:             savedState,
			ContainerImage:         props.ContainerImage,
			User:                   props.DockerUser,
			DockerClient:           p.dockerClient,
			ActionWorkingDirectory: workingDir,
			JailerRoot:             p.buildRoot,
		}
		c, err := firecracker.NewContainer(ctx, p.env, p.imageCacheAuth, task.GetExecutionTask(), opts)
		if err != nil {
			return nil, err
		}
		ctr = c
	case platform.SandboxContainerType:
		opts := &sandbox.Options{
			Network: props.DockerNetwork,
		}
		ctr = sandbox.New(opts)
	default:
		opts := &bare.Opts{
			EnableStats: *bareEnableStats,
		}
		ctr = bare.NewBareCommandContainer(opts)
	}
	return container.NewTracedCommandContainer(ctr), nil
}

func keyString(k *rnpb.RunnerKey) string {
	ph, err := platformHash(k.Platform)
	if err != nil {
		ph = "<ERR!>"
	}
	return fmt.Sprintf(
		"%s:%s:%s",
		k.GetGroupId(),
		truncate(k.InstanceName, 8, "..."),
		truncate(ph, 8, ""))
}

func (p *pool) String() string {
	return runnerSlice(p.runners).String()
}

// take finds the most recently used runner in the pool that matches the given
// query. If one is found, it is unpaused and returned.
func (p *pool) take(ctx context.Context, key *rnpb.RunnerKey) (*commandRunner, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	log.CtxInfof(ctx, "Looking for match for %q in runner pool %s", keyString(key), p)
	taskKeyBytes, err := proto.Marshal(key)
	if err != nil {
		return nil, status.InternalErrorf("failed to marshal runner key: %s", err)
	}

	for i := len(p.runners) - 1; i >= 0; i-- {
		r := p.runners[i]
		if key.GroupId != r.key.GroupId || r.state != paused {
			continue
		}

		// Check for an exact match on the runner pool keys.
		runnerKeyBytes, err := proto.Marshal(r.key)
		if err != nil {
			log.Errorf("Failed to marshal runner key for %s: %s", r, err)
			continue
		}
		if !bytes.Equal(taskKeyBytes, runnerKeyBytes) {
			continue
		}

		// TODO(bduffany): Find a way to unpause here without holding the lock.
		if err := r.Container.Unpause(ctx); err != nil {
			// If we fail to unpause, subsequent unpause attempts are also likely
			// to fail, so remove the container from the pool.
			p.remove(r)
			r.RemoveInBackground()
			return nil, status.WrapErrorf(err, "failed to unpause runner %s", r)
		}
		r.state = ready

		metrics.RunnerPoolCount.Dec()
		metrics.RunnerPoolDiskUsageBytes.Sub(float64(r.diskUsageBytes))
		metrics.RunnerPoolMemoryUsageBytes.Sub(float64(r.memoryUsageBytes))
		metrics.RecycleRunnerRequests.With(prometheus.Labels{
			metrics.RecycleRunnerRequestStatusLabel: hitStatusLabel,
		}).Inc()

		return r, nil
	}

	metrics.RecycleRunnerRequests.With(prometheus.Labels{
		metrics.RecycleRunnerRequestStatusLabel: missStatusLabel,
	}).Inc()

	return nil, nil
}

// RunnerCount returns the total number of runners in the pool.
func (p *pool) RunnerCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.runners)
}

// PausedRunnerCount returns the current number of paused runners in the pool.
func (p *pool) PausedRunnerCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.pausedRunnerCount()
}

// ActiveRunnerCount returns the number of non-paused runners in the pool.
func (p *pool) ActiveRunnerCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.runners) - p.pausedRunnerCount()
}

func (p *pool) pausedRunnerCount() int {
	n := 0
	for _, r := range p.runners {
		if r.state == paused {
			n++
		}
	}
	return n
}

func (p *pool) pausedRunnerMemoryUsageBytes() int64 {
	b := int64(0)
	for _, r := range p.runners {
		if r.state == paused {
			b += r.memoryUsageBytes
		}
	}
	return b
}

func (p *pool) stateFilePath() string {
	return filepath.Join(*rootDirectory, stateFileName)
}

func (p *pool) loadState(ctx context.Context) (*rnpb.RunnerPoolState, error) {
	b, err := disk.ReadFile(ctx, p.stateFilePath())
	if err != nil {
		return nil, status.WrapErrorf(err, "failed to read state file %s", p.stateFilePath())
	}
	state := &rnpb.RunnerPoolState{}
	if err := proto.Unmarshal(b, state); err != nil {
		return nil, status.WrapError(err, "failed to unmarshal state")
	}
	if err := os.Remove(p.stateFilePath()); err != nil {
		return nil, status.InternalErrorf("failed to remove state file %s: %s", p.stateFilePath(), err)
	}
	return state, nil
}

func (p *pool) saveState(ctx context.Context, state *rnpb.RunnerPoolState) error {
	if len(state.RunnerStates) == 0 {
		return nil
	}
	b, err := proto.Marshal(state)
	if err != nil {
		return status.WrapError(err, "failed to marshal state")
	}
	if _, err := disk.WriteFile(ctx, p.stateFilePath(), b); err != nil {
		return status.WrapErrorf(err, "failed to write %s", p.stateFilePath())
	}
	return nil
}

func (p *pool) initializeFromSavedState(ctx context.Context) error {
	state, err := p.loadState(ctx)
	if err != nil {
		if status.IsNotFoundError(err) {
			log.Infof("Runner state file not found at %s", p.stateFilePath())
			return nil
		}
		return err
	}
	for _, rs := range state.RunnerStates {
		nopTask := &repb.ExecutionTask{Command: &repb.Command{Platform: rs.GetRunnerKey().GetPlatform()}}
		props, err := p.effectivePlatform(nopTask)
		if err != nil {
			log.Warningf("Failed to restore runner state: %s", err)
			continue
		}
		r, err := p.newRunner(ctx, props, nil /*=scheduledTask*/, rs)
		if err != nil {
			log.Warningf("Failed to restore runner state: %s", err)
			continue
		}
		log.Infof("Restored runner %s from state", r)
	}
	log.Infof("Restored %d runner(s) from state file %s", len(p.runners), p.stateFilePath())
	return nil
}

// Shutdown removes all runners from the pool and prevents new ones from
// being added.
func (p *pool) Shutdown(ctx context.Context) error {
	p.mu.Lock()
	p.isShuttingDown = true
	var runnersToRemove []*commandRunner
	persistedState := &rnpb.RunnerPoolState{}
	if *contextBasedShutdown {
		// Remove only paused runners, since active runners should be removed only
		// after their currently assigned task is canceled due to the shutdown
		// grace period expiring.
		var pausedRunners, activeRunners []*commandRunner
		for _, r := range p.runners {
			if r.state == paused {
				pausedRunners = append(pausedRunners, r)
			} else {
				activeRunners = append(activeRunners, r)
			}
		}
		runnersToRemove = pausedRunners
		p.runners = activeRunners
		if len(runnersToRemove) > 0 {
			log.Infof("Runner pool: removing %s", runnerSlice(runnersToRemove))
		}

		for _, r := range pausedRunners {
			// TODO: figure out how/whether to preserve the workspace dir during
			// executor restarts, and remove this check. We exclude this check
			// for firecracker workflows because they don't use the workspace
			// disk.
			if r.PlatformProperties.PreserveWorkspace && !(r.PlatformProperties.WorkflowID != "" && r.PlatformProperties.WorkloadIsolationType == string(platform.FirecrackerContainerType)) {
				continue
			}

			containerState, err := r.Container.State(ctx)
			if status.IsUnimplementedError(err) {
				continue
			}
			if err != nil {
				log.Warningf("Failed to persist state for runner %s: %s", r, err)
				continue
			}
			runnerState := &rnpb.RunnerState{
				RunnerKey:      r.key,
				DebugId:        r.debugID,
				ContainerState: containerState,
			}
			persistedState.RunnerStates = append(persistedState.RunnerStates, runnerState)
			log.Infof("Persisting state for runner %s", r)
		}

	} else {
		runnersToRemove = p.runners
		p.runners = nil
		if len(runnersToRemove) > 0 {
			log.Infof("Runner pool: removing %s", runnerSlice(runnersToRemove))
		}
	}
	p.mu.Unlock()

	removeResults := make(chan error)
	for _, r := range runnersToRemove {
		// Remove runners in parallel, since each deletion is blocked on uploads
		// to finish (if applicable). A single runner that takes a long time to
		// upload its outputs should not block other runners from working on
		// workspace removal in the meantime.
		r := r
		go func() {
			removeResults <- r.RemoveWithTimeout(ctx)
		}()
	}

	// Write runner pool state file.
	if err := p.saveState(ctx, persistedState); err != nil {
		log.Errorf("Failed to save runner pool state: %s", err)
	} else {
		log.Infof("Wrote runner pool state to %s", p.stateFilePath())
	}

	// Now wait for runners to finish removing.
	errs := make([]error, 0)
	for range runnersToRemove {
		if err := <-removeResults; err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return status.InternalErrorf("failed to shut down runner pool: %s", errSlice(errs))
	}
	return nil
}

func (p *pool) Wait() {
	if *contextBasedShutdown {
		p.pendingRemovals.Wait()
	}
}

func (p *pool) remove(r *commandRunner) {
	for i := range p.runners {
		if p.runners[i] == r {
			// Not using the "swap with last element" trick here because we need to
			// preserve ordering.
			p.runners = append(p.runners[:i], p.runners[i+1:]...)
			break
		}
	}
}

func (p *pool) finalize(r *commandRunner) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.remove(r)
	r.RemoveInBackground()
}

// TryRecycle either adds r back to the pool if appropriate, or removes it,
// freeing up any resources it holds.
func (p *pool) TryRecycle(ctx context.Context, r interfaces.Runner, finishedCleanly bool) {
	ctx, cancel := background.ExtendContextForFinalization(ctx, runnerRecycleTimeout)
	defer cancel()

	cr, ok := r.(*commandRunner)
	if !ok {
		alert.UnexpectedEvent("unexpected_runner_type", "unexpected runner type %T", r)
		return
	}

	recycled := false
	defer func() {
		if !recycled {
			p.finalize(cr)
		}
	}()

	if !cr.PlatformProperties.RecycleRunner {
		return
	}
	if !finishedCleanly || cr.doNotReuse {
		log.CtxWarningf(ctx, "Failed to recycle runner %s due to previous execution error", cr)
		return
	}
	// Clean the workspace once before adding it to the pool (to save on disk
	// space).
	if err := cr.Workspace.Clean(); err != nil {
		log.CtxErrorf(ctx, "Failed to recycle runner %s: failed to clean workspace: %s", cr, err)
		return
	}
	if err := p.Add(ctx, cr); err != nil {
		if status.IsResourceExhaustedError(err) || status.IsUnavailableError(err) {
			log.CtxWarningf(ctx, "Failed to recycle runner %s: %s", cr, err)
		} else {
			// If not a resource limit exceeded error, probably it was an error
			// removing the directory contents or a docker daemon error.
			log.CtxErrorf(ctx, "Failed to recycle runner %s: %s", cr, err)
		}
		return
	}

	log.CtxInfof(ctx, "Successfully recycled runner %s", cr)
	recycled = true
}

func (p *pool) setLimits() {
	totalRAMBytes := int64(float64(resources.GetAllocatedRAMBytes()) * tasksize.MaxResourceCapacityRatio)
	estimatedRAMBytes := int64(float64(tasksize.DefaultMemEstimate) * runnerMemUsageEstimateMultiplierBytes)

	count := *maxRunnerCount
	if count == 0 {
		// Don't allow more paused runners than the max number of tasks that can be
		// executing at once, if they were all using the default memory estimate.
		if estimatedRAMBytes > 0 {
			count = int(float64(totalRAMBytes) / float64(estimatedRAMBytes))
		}
	} else if count < 0 {
		// < 0 means no limit.
		count = int(math.MaxInt32)
	}

	mem := *maxRunnerMemoryUsageBytes
	if mem > totalRAMBytes {
		mem = totalRAMBytes
	} else if mem < 0 {
		// < 0 means no limit.
		mem = math.MaxInt64
	}

	disk := *maxRunnerDiskSizeBytes
	if disk < 0 {
		// < 0 means no limit.
		disk = math.MaxInt64
	}

	p.maxRunnerCount = count
	p.maxRunnerMemoryUsageBytes = mem
	p.maxRunnerDiskUsageBytes = disk
	log.Infof(
		"Configured runner pool: max count=%d, max memory (per-runner, bytes)=%d, max disk (per-runner, bytes)=%d",
		p.maxRunnerCount, p.maxRunnerMemoryUsageBytes, p.maxRunnerDiskUsageBytes)
}

func platformHash(p *repb.Platform) (string, error) {
	// Note: we don't do any sort of canonicalization of the platform properties
	// (i.e. sorting by key), since in practice, bazel always sends us platform
	// properties sorted by key, and other clients are expected to send sorted
	// (or at least stable) platform properties as well.
	b, err := proto.Marshal(p)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(b)), nil
}

type labeledError struct {
	// Error is the wrapped error.
	Error error
	// Label is a short label for Prometheus.
	Label string
}

type errSlice []error

func (es errSlice) Error() string {
	if len(es) == 1 {
		return es[0].Error()
	}
	msgs := []string{}
	for _, err := range es {
		msgs = append(msgs, err.Error())
	}
	return fmt.Sprintf("[multiple errors: %s]", strings.Join(msgs, "; "))
}

func effectivePersistentWorkerKey(props *platform.Properties, commandArgs []string) string {
	if props.PersistentWorkerKey != "" {
		return props.PersistentWorkerKey
	}
	if !props.PersistentWorker {
		return ""
	}
	workerArgs, _ := SplitArgsIntoWorkerArgsAndFlagFiles(commandArgs)
	return strings.Join(workerArgs, " ")
}

func SplitArgsIntoWorkerArgsAndFlagFiles(args []string) ([]string, []string) {
	workerArgs := make([]string, 0)
	flagFiles := make([]string, 0)
	for _, arg := range args {
		if flagFilePattern.MatchString(arg) {
			flagFiles = append(flagFiles, arg)
		} else {
			workerArgs = append(workerArgs, arg)
		}
	}
	return workerArgs, flagFiles
}

func (r *commandRunner) supportsPersistentWorkers(ctx context.Context, command *repb.Command) bool {
	if r.PlatformProperties.PersistentWorkerKey != "" {
		return true
	}

	if !r.PlatformProperties.PersistentWorker {
		return false
	}

	_, flagFiles := SplitArgsIntoWorkerArgsAndFlagFiles(command.GetArguments())
	return len(flagFiles) > 0
}

func (r *commandRunner) startPersistentWorker(command *repb.Command, workerArgs, flagFiles []string) {
	// Note: Using the server context since this worker will stick around for
	// other tasks.
	ctx, cancel := context.WithCancel(r.env.GetServerContext())
	workerTerminated := make(chan struct{})
	r.stopPersistentWorker = func() error {
		// Canceling the worker context should terminate the worker process.
		cancel()
		// Wait for the worker to terminate. This is needed since canceling the
		// context doesn't block until the worker is killed. This helps ensure that
		// the worker is killed if we are shutting down. The shutdown case is also
		// why we use ExtendContextForFinalization here.
		ctx, cancel := background.ExtendContextForFinalization(r.env.GetServerContext(), persistentWorkerShutdownTimeout)
		defer cancel()
		select {
		case <-workerTerminated:
			return nil
		case <-ctx.Done():
			return status.DeadlineExceededError("Timed out waiting for persistent worker to shut down.")
		}
	}
	stdinReader, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter := io.Pipe()
	r.stdinWriter = stdinWriter
	r.stdoutReader = bufio.NewReader(stdoutReader)
	r.jsonDecoder = json.NewDecoder(r.stdoutReader)

	command = proto.Clone(command).(*repb.Command)
	command.Arguments = append(workerArgs, "--persistent_worker")

	go func() {
		defer close(workerTerminated)
		defer stdinReader.Close()
		defer stdoutWriter.Close()

		stdio := &container.Stdio{
			Stdin:  stdinReader,
			Stdout: stdoutWriter,
			Stderr: &r.stderr,
		}
		res := r.Container.Exec(ctx, command, stdio)
		log.Debugf("Persistent worker exited with response: %+v, flagFiles: %+v, workerArgs: %+v", res, flagFiles, workerArgs)
	}()
}

func (r *commandRunner) sendPersistentWorkRequest(ctx context.Context, command *repb.Command) *interfaces.CommandResult {
	// Clear any stderr that might be associated with a previous request.
	r.stderr.Reset()

	result := &interfaces.CommandResult{
		CommandDebugString: fmt.Sprintf("(persistentworker) %s", command.GetArguments()),
		ExitCode:           commandutil.NoExitCode,
	}

	workerArgs, flagFiles := SplitArgsIntoWorkerArgsAndFlagFiles(command.GetArguments())

	// If it's our first rodeo, create the persistent worker.
	if r.stopPersistentWorker == nil {
		r.startPersistentWorker(command, workerArgs, flagFiles)
	}

	r.doNotReuse = true

	// We've got a worker - now let's build a work request.
	requestProto := &wkpb.WorkRequest{
		Inputs: make([]*wkpb.Input, 0, len(r.Workspace.Inputs)),
	}

	expandedArguments, err := r.expandArguments(flagFiles)
	if err != nil {
		result.Error = status.WrapError(err, "expanding arguments")
		return result
	}
	requestProto.Arguments = expandedArguments

	// Collect all of the input digests
	for path, digest := range r.Workspace.Inputs {
		digestBytes, err := proto.Marshal(digest)
		if err != nil {
			result.Error = status.WrapError(err, "marshalling input digest")
			return result
		}
		requestProto.Inputs = append(requestProto.Inputs, &wkpb.Input{
			Digest: digestBytes,
			Path:   path,
		})
	}

	// Encode the work requests
	err = r.marshalWorkRequest(requestProto, r.stdinWriter)
	if err != nil {
		result.Error = status.UnavailableErrorf(
			"failed to send persistent work request: %s\npersistent worker stderr:\n%s",
			err, r.workerStderrDebugString())
		return result
	}

	// Now we've sent a work request, let's collect our response.
	responseProto := &wkpb.WorkResponse{}
	err = r.unmarshalWorkResponse(responseProto, r.stdoutReader)
	if err != nil {
		result.Error = status.UnavailableErrorf(
			"failed to read persistent work response: %s\npersistent worker stderr:\n%s",
			err, r.workerStderrDebugString())
		return result
	}

	// Populate the result from the response proto.
	result.Stderr = []byte(responseProto.Output)
	result.ExitCode = int(responseProto.ExitCode)
	r.doNotReuse = false
	return result
}

func (r *commandRunner) workerStderrDebugString() string {
	stderr, _ := r.stderr.ReadAll()
	str := string(stderr)
	if str == "" {
		return "<empty>"
	}
	return str
}

func (r *commandRunner) marshalWorkRequest(requestProto *wkpb.WorkRequest, writer io.Writer) error {
	protocol := r.PlatformProperties.PersistentWorkerProtocol
	if protocol == workerProtocolJSONValue {
		marshaler := &protojson.MarshalOptions{EmitUnpopulated: true}
		out, err := marshaler.Marshal(requestProto)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(writer, "%s\n", string(out))
		return err
	}
	if protocol != "" && protocol != workerProtocolProtobufValue {
		return status.FailedPreconditionErrorf("unsupported persistent worker type %s", protocol)
	}
	// Write the proto length (in varint encoding), then the proto itself
	buf := protowire.AppendVarint(nil, uint64(proto.Size(requestProto)))
	var err error
	buf, err = proto.MarshalOptions{}.MarshalAppend(buf, requestProto)
	if err != nil {
		return err
	}
	_, err = writer.Write(buf)
	return err
}

func (r *commandRunner) unmarshalWorkResponse(responseProto *wkpb.WorkResponse, reader io.Reader) error {
	protocol := r.PlatformProperties.PersistentWorkerProtocol
	if protocol == workerProtocolJSONValue {
		raw := json.RawMessage{}
		if err := r.jsonDecoder.Decode(&raw); err != nil {
			return err
		}
		return protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(raw, responseProto)
	}
	if protocol != "" && protocol != workerProtocolProtobufValue {
		return status.FailedPreconditionErrorf("unsupported persistent worker type %s", protocol)
	}
	// Read the response size from stdout as a unsigned varint.
	size, err := binary.ReadUvarint(r.stdoutReader)
	if err != nil {
		return err
	}
	data := make([]byte, size)
	// Read the response proto from stdout.
	if _, err := io.ReadFull(r.stdoutReader, data); err != nil {
		return err
	}
	if err := proto.Unmarshal(data, responseProto); err != nil {
		return err
	}
	return nil
}

// Recursively expands arguments by replacing @filename args with the contents of the referenced
// files. The @ itself can be escaped with @@. This deliberately does not expand --flagfile= style
// arguments, because we want to get rid of the expansion entirely at some point in time.
// Based on: https://github.com/bazelbuild/bazel/blob/e9e6978809b0214e336fee05047d5befe4f4e0c3/src/main/java/com/google/devtools/build/lib/worker/WorkerSpawnRunner.java#L324
func (r *commandRunner) expandArguments(args []string) ([]string, error) {
	expandedArgs := make([]string, 0)
	for _, arg := range args {
		if strings.HasPrefix(arg, "@") && !strings.HasPrefix(arg, "@@") && !externalRepositoryPattern.MatchString(arg) {
			file, err := os.Open(filepath.Join(r.Workspace.Path(), arg[1:]))
			if err != nil {
				return nil, err
			}
			defer file.Close()
			scanner := bufio.NewScanner(file)
			for scanner.Scan() {
				args, err := r.expandArguments([]string{scanner.Text()})
				if err != nil {
					return nil, err
				}
				expandedArgs = append(expandedArgs, args...)
			}
			if err := scanner.Err(); err != nil {
				return nil, err
			}
		} else {
			expandedArgs = append(expandedArgs, arg)
		}
	}

	return expandedArgs, nil
}

func truncate(text string, n int, truncateWith string) string {
	if len(text) > n {
		return text[:n] + truncateWith
	}
	return text
}
