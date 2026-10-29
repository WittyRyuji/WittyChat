from __future__ import annotations

import base64
import hashlib
import secrets
from urllib.parse import urlencode, urlsplit

import httpx
from joserfc import jwt
from joserfc.errors import JoseError
from joserfc.jwk import KeySet

from wittychat_server.config import Settings

ID_TOKEN_ALGORITHMS = ["RS256", "RS384", "RS512", "PS256", "ES256", "ES384", "EdDSA"]


class OidcError(Exception):
    pass


def pkce_pair() -> tuple[str, str]:
    verifier = secrets.token_urlsafe(48)
    challenge = base64.urlsafe_b64encode(hashlib.sha256(verifier.encode()).digest()).rstrip(b"=").decode()
    return verifier, challenge


def is_loopback_redirect(uri: str) -> bool:
    """Desktop clients must use an RFC 8252 loopback redirect on /callback."""
    parts = urlsplit(uri)
    return parts.scheme == "http" and parts.hostname in {"127.0.0.1", "localhost", "::1"} and parts.path == "/callback"


class OidcClient:
    def __init__(self, settings: Settings, http: httpx.AsyncClient) -> None:
        assert settings.oidc_issuer and settings.oidc_client_id
        self.settings = settings
        self.issuer = settings.oidc_issuer.rstrip("/")
        self.client_id = settings.oidc_client_id
        self.http = http
        self._metadata: dict | None = None
        self._keys: KeySet | None = None

    async def metadata(self) -> dict:
        if self._metadata is None:
            resp = await self.http.get(f"{self.issuer}/.well-known/openid-configuration")
            resp.raise_for_status()
            data = resp.json()
            if str(data.get("issuer", "")).rstrip("/") != self.issuer:
                raise OidcError("issuer in discovery document does not match configuration")
            self._metadata = data
        return self._metadata

    async def _key_set(self, refresh: bool = False) -> KeySet:
        if self._keys is None or refresh:
            resp = await self.http.get((await self.metadata())["jwks_uri"])
            resp.raise_for_status()
            self._keys = KeySet.import_key_set(resp.json())
        return self._keys

    async def authorization_url(self, *, redirect_uri: str, state: str, nonce: str, code_challenge: str) -> str:
        params = {
            "response_type": "code",
            "client_id": self.client_id,
            "redirect_uri": redirect_uri,
            "scope": self.settings.oidc_scopes,
            "state": state,
            "nonce": nonce,
            "code_challenge": code_challenge,
            "code_challenge_method": "S256",
        }
        return f"{(await self.metadata())['authorization_endpoint']}?{urlencode(params)}"

    async def exchange_code(self, *, code: str, code_verifier: str, redirect_uri: str) -> dict:
        data = {
            "grant_type": "authorization_code",
            "code": code,
            "redirect_uri": redirect_uri,
            "code_verifier": code_verifier,
            "client_id": self.client_id,
        }
        auth = (self.client_id, self.settings.oidc_client_secret) if self.settings.oidc_client_secret else None
        resp = await self.http.post((await self.metadata())["token_endpoint"], data=data, auth=auth)
        if resp.status_code != 200:
            raise OidcError(f"token endpoint returned HTTP {resp.status_code}")
        return resp.json()

    async def verify_id_token(self, id_token: str, nonce: str) -> dict:
        try:
            try:
                token = jwt.decode(id_token, await self._key_set(), algorithms=ID_TOKEN_ALGORITHMS)
            except JoseError:
                # Possibly a rotated signing key: refetch JWKS once.
                token = jwt.decode(id_token, await self._key_set(refresh=True), algorithms=ID_TOKEN_ALGORITHMS)
            jwt.JWTClaimsRegistry(
                iss={"essential": True, "value": (await self.metadata())["issuer"]},
                aud={"essential": True, "value": self.client_id},
                sub={"essential": True},
                exp={"essential": True},
                nonce={"essential": True, "value": nonce},
            ).validate(token.claims)
        except (JoseError, ValueError) as exc:
            raise OidcError(f"invalid id_token: {exc}") from exc
        return dict(token.claims)

    async def userinfo(self, access_token: str) -> dict:
        endpoint = (await self.metadata()).get("userinfo_endpoint")
        if not endpoint:
            return {}
        resp = await self.http.get(endpoint, headers={"Authorization": f"Bearer {access_token}"})
        return resp.json() if resp.status_code == 200 else {}

    async def login(self, *, code: str, code_verifier: str, redirect_uri: str, nonce: str) -> dict:
        """Redeem an authorization code and return verified claims (id_token + userinfo)."""
        tokens = await self.exchange_code(code=code, code_verifier=code_verifier, redirect_uri=redirect_uri)
        if "id_token" not in tokens:
            raise OidcError("token response has no id_token")
        claims = await self.verify_id_token(tokens["id_token"], nonce)
        if access_token := tokens.get("access_token"):
            info = await self.userinfo(access_token)
            if info.get("sub") == claims["sub"]:
                claims = {**info, **claims}
        return claims

    def check_allowed(self, claims: dict) -> None:
        allowed = self.settings.allowed_groups
        if allowed and not set(claims.get("groups") or []) & set(allowed):
            raise OidcError("user is not in an allowed group")
