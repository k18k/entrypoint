# Changesets

Every user-visible change (gateway, CLI, connector, SDK) adds a changeset:

```bash
npm run changeset
```

Pick `patch` for fixes, `minor` for new features or behavior changes, and write the summary for users: it goes into CHANGELOG.md and the GitHub release notes as is.

Releasing: `npm run version` bumps the version, writes CHANGELOG.md and syncs versions.json, scripts/install.sh and sdk/node. Commit, tag `v<version>`, push the tag.
