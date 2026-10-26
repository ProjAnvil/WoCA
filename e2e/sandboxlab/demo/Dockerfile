# sandbox-demo — the EE sandbox plane's acceptance container (compose
# profile "demo", spec §12): python stdlib only (HTTP + JSON against the
# stack's internal surfaces), the shared workspaces volume mounted
# read-only by the compose service. The exit code is the verdict.
FROM python:3.12-alpine
COPY demo_assert.py /demo/demo_assert.py
ENTRYPOINT ["python3", "/demo/demo_assert.py"]
