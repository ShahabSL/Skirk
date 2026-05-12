# Archived Cloud Run Probe

This was a temporary research probe for measuring whether a real `*.run.app`
endpoint worked through the restricted network. It is not part of the current
Skirk production setup, which uses the Drive `appDataFolder` transport described
in the root README and `docs/`.

Endpoints:

- `/healthz` - returns HTTP 204.
- `/headers` - returns request metadata without auth/cookie headers.
- `/stream` - emits delayed chunks.
- `/ws` - WebSocket echo.

If you deploy this historical probe manually, track and delete the cloud
resources yourself. The normal Skirk install/setup flow does not create Cloud Run
services.
