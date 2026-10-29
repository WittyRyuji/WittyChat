from __future__ import annotations

import asyncio
import contextlib
import logging
from collections.abc import AsyncIterator
from contextlib import asynccontextmanager
from pathlib import Path

import httpx
from fastapi import FastAPI
from fastapi.staticfiles import StaticFiles
from starlette.middleware.sessions import SessionMiddleware

from wittychat_server import admin, api, ws
from wittychat_server.admin.routes import router as admin_router
from wittychat_server.broadcast import Dispatcher, Hub, PgListener
from wittychat_server.config import Settings, get_settings
from wittychat_server.db import make_engine, make_sessionmaker
from wittychat_server.directory import directory_loop
from wittychat_server.instance import ensure_instance_id
from wittychat_server.oidc import OidcClient
from wittychat_server.storage import make_storage

log = logging.getLogger("wittychat")


def create_app(settings: Settings | None = None) -> FastAPI:
    settings = settings or get_settings()

    @asynccontextmanager
    async def lifespan(app: FastAPI) -> AsyncIterator[None]:
        engine = make_engine(settings.database_url)
        sessionmaker = make_sessionmaker(engine)
        http = httpx.AsyncClient(timeout=15)
        hub = Hub()
        dispatcher = Dispatcher(sessionmaker, hub, use_pg=settings.use_pg_broadcast)
        app.state.engine = engine
        app.state.sessionmaker = sessionmaker
        app.state.http = http
        app.state.hub = hub
        app.state.dispatcher = dispatcher
        app.state.oidc = OidcClient(settings, http) if settings.oidc_enabled else None
        app.state.storage = make_storage(settings)

        await ensure_instance_id(sessionmaker)
        await dispatcher.start()
        tasks = [asyncio.create_task(dispatcher.run(), name="dispatcher")]
        if settings.use_pg_broadcast:
            tasks.append(asyncio.create_task(PgListener(settings.database_url, dispatcher).run(), name="pg-listener"))
        if settings.oidc_enabled and settings.pocketid_api_key:
            tasks.append(
                asyncio.create_task(directory_loop(settings, sessionmaker, http, dispatcher), name="directory-sync")
            )
        if settings.dev_login:
            log.warning("DEV LOGIN IS ENABLED - anyone can sign in as any user. Never enable this in production.")
        try:
            yield
        finally:
            for task in tasks:
                task.cancel()
            for task in tasks:
                with contextlib.suppress(asyncio.CancelledError, Exception):
                    await task
            await http.aclose()
            await engine.dispose()

    app = FastAPI(title="WittyChat", version="0.1.0", lifespan=lifespan)
    app.state.settings = settings
    app.add_middleware(
        SessionMiddleware,
        secret_key=settings.secret_key,
        session_cookie="wittychat_admin",
        max_age=12 * 3600,
        same_site="lax",
        https_only=settings.public_url.startswith("https://"),
    )
    app.include_router(api.router)
    app.include_router(ws.router)
    app.include_router(admin_router)
    app.mount(
        "/admin/static", StaticFiles(directory=Path(admin.__file__).parent / "static"), name="admin-static"
    )

    @app.get("/healthz", include_in_schema=False)
    async def healthz() -> dict[str, str]:
        return {"status": "ok"}

    return app
