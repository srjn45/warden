#1 Limit Exhausted:

there should be a flag at backend level to mark it limit_exhausted
this should be auto detected by warden as well, but we should also provide human to manually mark backend as limit_exhausted
the reason is, if automated system is wrong or delayed then it can still keep on spawning exhausted backend


#2 limit tracking (autonomous limit exhaustion & restore tracker):
also we need to build backend specific implementation to understand how each backend's limit is working and when will it restore next, claude & antigravity have /usage command while codex have /status command to check this limit usage as well as when will it be restored

#3 fix & improve manual agent spawning using TUI
when a new agent is getting spawned, ask for following things
*name: 
*role:
 tier:     // optional if unset then use defaul based on role, if set then choose backend & model based on tier config
 backend:  // optional if unset then choose as per tier, if set then choose model as per tier but only for the specified backend
 model:    // optional if unset then as per previous config, if set then use specific model ignoring tier, if backend & model are not matching then return error

tier/backend/model are more for advanced user & should be optional if left empty then should fallback to role





