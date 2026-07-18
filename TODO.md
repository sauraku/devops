# DevOps Control TODO

## Environment configuration integrity

- [ ] Replace destructive environment-map replacement with explicit patch semantics:
  - Preserve saved values for keys omitted from an update request.
  - Require an explicit `clear` operation to remove a saved value.
  - Record an audit event containing only added, updated, and cleared key names (never values).
  - Add regression coverage for a full Medusa SMTP bulk save followed by a partial update, proving all SMTP values remain available to deployment.
