import { grp } from "../../proto/group_ts_proto";
import { user_id } from "../../proto/user_id_ts_proto";
import { BuildBuddyServiceRpcName } from "../service/rpc_service";

export class User {
  displayUser: user_id.DisplayUser;
  groups: grp.Group[];
  selectedGroup: grp.Group;
  allowedRpcs: Set<BuildBuddyServiceRpcName>;
  githubToken: string;
  /** Whether the user is temporarily acting as a member of the selected group. */
  isImpersonating: boolean;

  constructor(init: Partial<User>) {
    this.displayUser = init.displayUser!;
    this.groups = init.groups!;
    this.selectedGroup = init.selectedGroup!;
    this.allowedRpcs = init.allowedRpcs!;
    this.githubToken = init.githubToken!;
    this.isImpersonating = init.isImpersonating!;

    // All props are required, but it's a pain in TS to get a type representing
    // "only the fields of User, not the methods". So do a runtime check here.
    for (const prop of Object.getOwnPropertyNames(this) as Array<keyof User>) {
      if (this[prop] === undefined || this[prop] === null) {
        throw new Error(`${prop} property is required`);
      }
    }
  }

  getId() {
    return this.displayUser.userId?.id || "";
  }

  selectedGroupName() {
    if (this.selectedGroup?.name == "DEFAULT_GROUP") return "Organization";
    return this.selectedGroup?.name?.trim();
  }

  canCall(rpc: BuildBuddyServiceRpcName) {
    return this.allowedRpcs.has(rpc);
  }

  canImpersonate() {
    return this.allowedRpcs.has("getInvocationOwner");
  }

  isGroupAdmin() {
    return this.allowedRpcs.has("updateGroup");
  }
}
