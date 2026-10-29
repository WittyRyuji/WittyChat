from __future__ import annotations

import logging
from typing import Annotated

from fastapi import APIRouter, Depends, HTTPException, Request
from sqlalchemy.exc import IntegrityError

from wittychat_server import models
from wittychat_server.db import utcnow
from wittychat_server.deps import DB, AppSettings, CurrentUser, commit, current_session
from wittychat_server.oidc import OidcError, is_loopback_redirect
from wittychat_server.security import create_session
from wittychat_server.serializers import user_out
from wittychat_server.users import Profile, profile_from_claims, upsert_identity_user
from wittychat_shared import protocol as p

log = logging.getLogger(__name__)
router = APIRouter(tags=["auth"])


@router.get("/auth/config", response_model=p.AuthConfig)
async def auth_config(request: Request, settings: AppSettings) -> p.AuthConfig:
    oidc = request.app.state.oidc
    if oidc is None:
        return p.AuthConfig(oidc_enabled=False, dev_login=settings.dev_login)
    try:
        metadata = await oidc.metadata()
    except Exception as exc:
        log.warning("OIDC discovery failed: %s", exc)
        raise HTTPException(503, "identity provider unavailable") from exc
    return p.AuthConfig(
        oidc_enabled=True,
        issuer=metadata["issuer"],
        client_id=settings.oidc_client_id,
        authorization_endpoint=metadata["authorization_endpoint"],
        scopes=settings.oidc_scopes,
        dev_login=settings.dev_login,
    )


async def _login(request: Request, db: DB, provider: str, subject: str, profile: Profile) -> p.LoginResponse:
    settings = request.app.state.settings
    for attempt in range(2):
        try:
            user = await upsert_identity_user(db, settings, provider, subject, profile, bootstrap_admin=True)
            if not user.is_active:
                raise HTTPException(403, "account is disabled")
            token = await create_session(
                db,
                user,
                settings,
                user_agent=request.headers.get("user-agent"),
                ip=request.client.host if request.client else None,
            )
            await commit(request, db)
            return p.LoginResponse(token=token, user=user_out(user))
        except IntegrityError:
            # Concurrent first login of the same identity; the retry finds the row.
            await db.rollback()
            if attempt:
                raise
    raise AssertionError("unreachable")


@router.post("/auth/oidc/exchange", response_model=p.LoginResponse)
async def oidc_exchange(req: p.OidcExchangeRequest, request: Request, db: DB, settings: AppSettings) -> p.LoginResponse:
    oidc = request.app.state.oidc
    if oidc is None:
        raise HTTPException(404, "OIDC login is not configured")
    if not is_loopback_redirect(req.redirect_uri):
        raise HTTPException(400, "redirect_uri must be a loopback /callback URL")
    try:
        claims = await oidc.login(
            code=req.code, code_verifier=req.code_verifier, redirect_uri=req.redirect_uri, nonce=req.nonce
        )
        oidc.check_allowed(claims)
    except OidcError as exc:
        log.info("OIDC login rejected: %s", exc)
        raise HTTPException(401, str(exc)) from exc
    return await _login(request, db, settings.oidc_provider_name, claims["sub"], profile_from_claims(claims))


@router.post("/auth/dev-login", response_model=p.LoginResponse)
async def dev_login(req: p.DevLoginRequest, request: Request, db: DB, settings: AppSettings) -> p.LoginResponse:
    if not settings.dev_login:
        raise HTTPException(404, "not found")
    profile = Profile(username=req.username, display_name=req.display_name or req.username)
    return await _login(request, db, "dev", req.username.lower(), profile)


@router.post("/auth/logout", status_code=204)
async def logout(
    request: Request, db: DB, session: Annotated[tuple[models.Session, models.User], Depends(current_session)]
) -> None:
    session[0].revoked_at = utcnow()
    await commit(request, db)


@router.get("/me", response_model=p.User)
async def me(user: CurrentUser) -> p.User:
    return user_out(user)
