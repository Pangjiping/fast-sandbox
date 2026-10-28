---
title: Contributing FSEPs
description: Discuss first, write proposals when needed, and focus review on compatibility, testing, and rollback.
---

# Contributing FSEPs

Fast Sandbox Enhancement Proposals (FSEPs) record major designs that need shared agreement. The process draws on the [Go Proposal Process](https://github.com/golang/proposal) for discussing ideas before writing full designs, and the [Kubernetes KEP template](https://github.com/kubernetes/enhancements/blob/master/keps/NNNN-kep-template/README.md) for compatibility, testing, and rollback considerations. The directory layout and numbering follow [OpenSandbox OSEPs](https://github.com/opensandbox-group/OpenSandbox/tree/main/oseps).

## When a proposal is needed

For major features, architectural changes across components, or changes affecting public interfaces, core lifecycle behavior, or security isolation, start an Issue with a few sentences describing the problem, approach, and impact. Authors and maintainers decide there whether an FSEP is needed. Write a full proposal for complex tradeoffs, compatibility concerns, or coordination across components. If the Issue provides enough detail, proceed directly to an implementation PR and retain the decision rationale in the discussion.

Small bug fixes, documentation corrections, internal refactoring, and compatible local improvements can go directly to a PR without an additional Issue or FSEP. A major design that has already been discussed sufficiently can also go directly to an FSEP PR without opening a separate Issue.

## Minimal workflow

1. **Discuss and design**: When needed, use the [template](fsep-template.md.template) and submit a PR containing the proposal and an [index](README.md) entry. Link existing discussions. Include enough detail to support a decision; there is no length requirement, and additional detail is optional.
2. **Review and decide**: Maintainers of the affected components review the approach in the PR, focusing on the three requirements below. Set the status to `accepted` after approval, or `closed` if declined or withdrawn, retaining the reason in the proposal. Merging a `draft` preserves the draft and does not approve the design.
3. **Implement and complete**: Link implementation PRs to the FSEP and track progress directly in Issues or PRs. Once the agreed scope, tests, and relevant `docs/` updates are complete, set the status to `implemented` in the implementation PR. Acceptance does not imply scheduling, and implementation does not imply GA availability.

| Required topic | Minimum detail |
|----------------|----------------|
| Compatibility | Explain the impact on existing APIs, CRDs, SDKs, CLI commands, configuration, and existing resources or snapshots. Describe supported version combinations when components may run different versions. Breaking changes require an explicit impact assessment, a migration path, and explicit maintainer agreement. |
| Testing | Provide executable validation steps and acceptance criteria. Cover normal operation, failure recovery, and compatibility according to risk. For performance work, state the measurement boundary, environment, and cache state. Choose only the relevant test levels. |
| Rollout and rollback | Explain enablement or staged rollout, signals to monitor, rollback triggers, and how to disable the feature or downgrade. Describe how existing resources, data, or snapshots are handled after rollback. If safe rollback is unavailable, state the limits and recovery plan. |

Keep all three topics; if one does not apply, explain why in one sentence. Regular maintainer PR review is sufficient, with no separate committee, fixed waiting period, or release approval form. Significant design changes require another review and updates to the proposal and index.

## Status

The usual path is `draft` → `accepted` → `implemented`. A proposal that has not been implemented may move to `closed`. There are no separate states for agreement in principle, awaiting implementation, or work in progress.

| Status | Meaning |
|--------|---------|
| `draft` | Under discussion and not yet approved. |
| `accepted` | Maintainers have approved the design for implementation; progress is tracked in linked Issues or PRs. |
| `implemented` | The agreed scope, tests, and relevant user documentation are complete. |
| `closed` | Declined, withdrawn, or no longer pursued. Record the reason and link any replacement proposal. |

## Creating and maintaining proposals

Run from the repository root:

```bash
./fseps/init-fsep.sh "Snapshot Distribution"
./fseps/init-fsep.sh --author "@username" --slug snapshot-distribution "Improve Snapshot Distribution"
./fseps/init-fsep.sh --help
```

Write proposal titles and content in English. The script fills in metadata, defaults to `draft`, and creates the next `NNNN-english-slug.md` file based on the highest number in `fseps/`. Use `--slug` to override the filename slug. The default author is taken from the repository's Git `user.name`, then `user.email`, then the current username. The script can be called from any working directory.

Update the index when adding a proposal. Update `last-updated` and the index when changing its content or status. If concurrent PRs use the same number, the later PR must adjust it before merging. Never reuse a merged number, and retain closed proposals.

`fseps/` records plans and decisions; `docs/` describes current product behavior. Update the relevant documentation during implementation and link to it from the proposal. Temporary notes remain in the Git-ignored `.dev/` directory; formal discussions must not depend on those files. Changes involving OpenSandbox integration may link an OSEP, with each repository reviewing its own changes.
