from __future__ import annotations

import argparse
from pathlib import Path

from wittychat_server.config import get_settings


def alembic_config():
    from alembic.config import Config

    cfg = Config()
    cfg.set_main_option("script_location", str(Path(__file__).parent / "migrations"))
    cfg.set_main_option("sqlalchemy.url", get_settings().database_url.replace("%", "%%"))
    return cfg


def main() -> None:
    parser = argparse.ArgumentParser(prog="wittychat-server")
    sub = parser.add_subparsers(dest="command", required=True)
    sub.add_parser("migrate", help="apply database migrations")
    serve = sub.add_parser("serve", help="run the HTTP/WebSocket server")
    serve.add_argument("--host", default="127.0.0.1")
    serve.add_argument("--port", type=int, default=8000)
    serve.add_argument("--workers", type=int, default=1)
    serve.add_argument("--reload", action="store_true")
    args = parser.parse_args()

    if args.command == "migrate":
        from alembic import command

        command.upgrade(alembic_config(), "head")
    elif args.command == "serve":
        import uvicorn

        uvicorn.run(
            "wittychat_server.main:create_app",
            factory=True,
            host=args.host,
            port=args.port,
            workers=args.workers,
            reload=args.reload,
            proxy_headers=True,
            forwarded_allow_ips="*",
        )


if __name__ == "__main__":
    main()
