#!/app/.venv/bin/python
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Entrypoint of the CetiRun AI Gateway image.

``cetirun-ai-gateway migrate`` applies LiteLLM's database migrations.
``cetirun-ai-gateway copilot-proxy ...`` runs the per-Agent GitHub Copilot
proxy with the remaining arguments. Any other arguments start the LiteLLM
proxy with them.

Starting the proxy never touches a database by itself. The same image serves
the shared Gateway, which has PostgreSQL, and the per-agent Copilot proxy, which
has none, so migrating is a separate, explicit step.

The Gateway proxy runs in this process, with the image's guard callback, and,
with CETIRUN_REQUEST_LOG set, its request log callback, registered before
LiteLLM loads its configuration (see ``gateway``).
"""

import os
import pathlib
import signal
import subprocess
import sys
import time
from urllib.parse import parse_qsl, urlencode, urlsplit, urlunsplit

# Serialises migrations across pods. Distinct from the key prisma locks itself.
MIGRATION_LOCK_KEY = 0x4345544952554E  # "CETIRUN"

# Query parameters libpq understands with the same meaning prisma gives them.
# The rest, such as connection_limit or schema, are prisma's own.
LIBPQ_PARAMETERS = {"sslmode", "connect_timeout", "application_name"}

# An address nothing listens on. The Copilot proxy points LiteLLM's own
# device-code sign-in here: signing in is the Portal's job, and a request that
# reached GitHub from the proxy would hang waiting for a user who is not there.
DISABLED_URL = "http://127.0.0.1:9/disabled"

# Set by the Gateway chart. CETIRUN_REQUEST_LOG=true turns the request log on;
# CETIRUN_SPEND_LOGS=false then also turns LiteLLM's own row per call off.
REQUEST_LOG_ENV = "CETIRUN_REQUEST_LOG"
SPEND_LOGS_ENV = "CETIRUN_SPEND_LOGS"
# Ways of starting the proxy other than one uvicorn server in this process.
# Their workers would load LiteLLM afresh, without the callbacks.
OTHER_SERVERS = ("--run_gunicorn", "--run_hypercorn", "--run_granian", "--reload")


def migrations_schema():
    """Locate LiteLLM's schema by the migrations beside it.

    Several copies of schema.prisma exist in a LiteLLM installation and only
    some have a migrations directory next to them. Pointed at one without,
    ``prisma migrate deploy`` finds nothing to apply, exits zero, and leaves an
    empty database behind a successful install. So a schema is accepted only
    when migrations sit beside it.
    """
    import litellm_proxy_extras

    directory = pathlib.Path(litellm_proxy_extras.__file__).parent
    schema = directory / "schema.prisma"
    migrations = directory / "migrations"
    count = sum(1 for entry in migrations.iterdir() if entry.is_dir()) if migrations.is_dir() else 0
    if not schema.is_file() or count == 0:
        sys.exit(f"no schema.prisma with migrations beside it in {directory}")
    return schema, count


def libpq_url(url):
    """The DATABASE_URL, reduced to what libpq accepts."""
    parts = urlsplit(url)
    query = [(key, value) for key, value in parse_qsl(parts.query) if key in LIBPQ_PARAMETERS]
    return urlunsplit(parts._replace(query=urlencode(query)))


def connect(url, deadline):
    """Connect, waiting for a database that is still starting.

    The bundled PostgreSQL starts in the same release as the Gateway, so the
    first pods usually get here before it accepts connections.
    """
    import psycopg

    while True:
        try:
            return psycopg.connect(libpq_url(url), autocommit=True, connect_timeout=5)
        except psycopg.OperationalError as error:
            if time.monotonic() >= deadline:
                sys.exit(f"database not reachable: {error}")
            print(f"waiting for the database: {str(error).strip().splitlines()[0]}", flush=True)
            time.sleep(3)


def migrate():
    # This process is PID 1 of its container, and the kernel ignores signals a
    # PID 1 has no handler for. Without this, a pod stopped while waiting for
    # its database would sit out the whole termination grace period.
    signal.signal(signal.SIGTERM, lambda *_: sys.exit(128 + signal.SIGTERM))

    url = os.environ.get("DATABASE_URL")
    if not url:
        sys.exit("DATABASE_URL is not set")
    deadline = time.monotonic() + int(os.environ.get("MIGRATE_WAIT_SECONDS", "600"))
    connection = connect(url, deadline)

    # Only one pod migrates at a time. prisma's own advisory lock is not enough:
    # a second prisma waits for it inside an open transaction, and LiteLLM's
    # migrations include CREATE INDEX CONCURRENTLY, which waits for every open
    # transaction. The two wait on each other, and PostgreSQL resolves it by
    # killing one - possibly the one migrating, leaving a failed migration that
    # blocks every later deploy. Polling with pg_try_advisory_lock keeps each
    # attempt a short transaction, and the holder's connection stays idle, so
    # neither blocks the index build.
    while not connection.execute("SELECT pg_try_advisory_lock(%s)", (MIGRATION_LOCK_KEY,)).fetchone()[0]:
        if time.monotonic() >= deadline:
            sys.exit("timed out waiting for another pod's migration to finish")
        print("another pod is migrating; waiting", flush=True)
        time.sleep(2)

    schema, count = migrations_schema()
    print(f"applying migrations from {schema} ({count} migrations)", flush=True)
    result = subprocess.run(["prisma", "migrate", "deploy", "--schema", str(schema)], check=False)
    if result.returncode == 0:
        build_request_log_indexes()
    connection.close()
    sys.exit(result.returncode)


def build_request_log_indexes():
    """Build the indexes on LiteLLM's request log that its migrations leave out.

    LiteLLM 1.105 builds two indexes on LiteLLM_SpendLogs after `prisma migrate
    deploy` rather than in a migration, so that building them never blocks
    inserts: its own migration job does it, or a proxy that migrated itself.
    The Gateway's proxies start with schema updates disabled, so this step
    builds them. It holds no transaction open, as the index build needs.

    The proxy serves without them, only with slower request log queries, so a
    build that could not finish does not fail the migration; the next pod's
    migration tries again. A LiteLLM without the step has nothing to build.
    """
    try:
        from litellm_proxy_extras.utils import ProxyExtrasDBManager
    except ImportError:
        return
    build = getattr(ProxyExtrasDBManager, "build_request_log_indexes", None)
    if build is None:
        return
    try:
        built = build()
    except Exception as error:  # LiteLLM promises not to raise; a failure here must not fail the migration
        built = False
        print(f"building the request log indexes failed: {error}", flush=True)
    if built:
        print("request log indexes are in place", flush=True)
    else:
        print("the request log indexes are not all in place; the next migration tries again", flush=True)


def token_present(path):
    try:
        return bool(pathlib.Path(path).read_text(encoding="utf-8").strip())
    except OSError:
        return False


def copilot_proxy_environment(token_file, environ):
    """The environment LiteLLM's github_copilot provider needs.

    It reads the owner's GitHub token from the file the Portal's Secret
    provides and exchanges it for short-lived Copilot tokens, which it caches
    in GITHUB_COPILOT_TOKEN_DIR until they expire, without reading the GitHub
    token again meanwhile. The operator restarts the proxy for every new
    sign-in, so the cache never outlives the account it came from.
    """
    env = dict(environ)
    env["GITHUB_COPILOT_ACCESS_TOKEN_FILE"] = token_file
    env.setdefault("GITHUB_COPILOT_TOKEN_DIR", "/var/cache/copilot")
    env["GITHUB_COPILOT_DEVICE_CODE_URL"] = DISABLED_URL
    env["GITHUB_COPILOT_ACCESS_TOKEN_URL"] = DISABLED_URL
    # The proxy's own price table is irrelevant, the Gateway prices every call,
    # and fetching it from the internet at start is not wanted.
    env.setdefault("LITELLM_LOCAL_MODEL_COST_MAP", "True")
    return env


def copilot_proxy(args):
    """Run the per-Agent GitHub Copilot proxy once the owner has signed in.

    Until the token exists there is nothing to serve, so the proxy waits and
    stays unready; the operator registers the Agent's Copilot routes only once
    the proxy is ready.
    """
    signal.signal(signal.SIGTERM, lambda *_: sys.exit(128 + signal.SIGTERM))
    token_file = os.environ.get("COPILOT_TOKEN_FILE", "/var/run/copilot/access-token")
    interval = float(os.environ.get("COPILOT_TOKEN_POLL_SECONDS", "2"))
    waiting = False
    while not token_present(token_file):
        if not waiting:
            print(f"waiting for the GitHub Copilot sign-in at {token_file}", flush=True)
            waiting = True
        time.sleep(interval)
    print("GitHub Copilot token present; starting the proxy", flush=True)
    os.execvpe("litellm", ["litellm", *args], copilot_proxy_environment(token_file, os.environ))


def worker_count(args, environ):
    """The number of proxy workers LiteLLM would start with these arguments."""
    count = environ.get("NUM_WORKERS", "1")
    for index, arg in enumerate(args):
        if arg == "--num_workers" and index + 1 < len(args):
            count = args[index + 1]
        elif arg.startswith("--num_workers="):
            count = arg.split("=", 1)[1]
    try:
        return int(count)
    except ValueError:
        return 1  # LiteLLM rejects it itself


def gateway(args, environ=None):
    """Start the LiteLLM proxy with the guard, and the request log when the chart asks for it.

    The guard callback is always registered: it drops request fields that would
    change the Gateway for one call, keeps per-Agent GitHub Copilot routes to
    the keys that hold them, keeps operator-issued keys to the deployments of
    their access groups, also when an auto router picks the model, refuses a
    request for a decision model through any API but the Decisions API,
    shortens a router's request to a decision model to fit its context, removes
    encrypted reasoning without the Gateway's mark while encrypted content
    affinity is on, writes that mark in a form clients keep, ends fallbacks
    after a deployment refused a request's encrypted reasoning, removes the
    sampling parameters a reasoning model refuses from requests an auto router
    sends it, and keeps LiteLLM's list of deployments in cooldown out of the
    errors callers get
    (cetirun_ai_gateway/guard.py).

    The callbacks are registered here, in code, rather than in
    litellm_settings.callbacks. A configured callback depends on how LiteLLM
    combines its configuration file with settings saved in its database, where
    a saved callbacks list can replace the configured one, and an image without
    the module cannot start with a configuration that names it, as after a
    rollback. Registered here, the callbacks depend on neither, and an image
    built before the request log ignores these variables and starts with the
    chart's configuration.

    LiteLLM's own row per call is turned off only here, once the request log
    callback is registered, never on its own: no configuration leaves a Gateway
    without a record of each call. LiteLLM reads the setting when it loads its
    configuration and keeps this value unless general_settings sets one.

    The callbacks are registered in this process, so the proxy has to run in
    it: one uvicorn server, as LiteLLM starts by default.
    """
    environ = os.environ if environ is None else environ
    request_log = environ.get(REQUEST_LOG_ENV, "").strip().lower() == "true"
    spend_logs_off = environ.get(SPEND_LOGS_ENV, "").strip().lower() == "false"
    if worker_count(args, environ) > 1 or any(arg.split("=", 1)[0] in OTHER_SERVERS for arg in args):
        sys.exit("the Gateway's callbacks need the proxy in one uvicorn process: workers started otherwise "
                 "would run without them. Scale with replicas instead.")

    import litellm
    from cetirun_ai_gateway.guard import handler as guard

    litellm.logging_callback_manager.add_litellm_callback(guard)
    print("guard callback registered", flush=True)
    if request_log:
        from cetirun_ai_gateway.request_log import handler

        litellm.logging_callback_manager.add_litellm_callback(handler)
        print("request log callback registered", flush=True)
        if spend_logs_off:
            from litellm.proxy import proxy_server

            if not hasattr(proxy_server, "disable_spend_logs"):
                sys.exit("this LiteLLM has no proxy_server.disable_spend_logs; the spend log cannot be turned off")
            proxy_server.disable_spend_logs = True
            print("LiteLLM's spend log is off; the request log records each call", flush=True)
    elif spend_logs_off:
        print(f"{SPEND_LOGS_ENV}=false is ignored without {REQUEST_LOG_ENV}=true: "
              "LiteLLM keeps its spend log while nothing replaces it", flush=True)

    from litellm.proxy.proxy_cli import run_server

    run_server.main(args=list(args), prog_name="litellm")


def main():
    main_with(sys.argv[1:])


def main_with(args):
    if args[:1] == ["migrate"]:
        migrate()
    if args[:1] == ["copilot-proxy"]:
        copilot_proxy(args[1:])
    gateway(args)


if __name__ == "__main__":
    main()
