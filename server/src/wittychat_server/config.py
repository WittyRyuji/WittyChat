from __future__ import annotations

from functools import lru_cache
from pathlib import Path
from typing import Literal

from pydantic import Field
from pydantic_settings import BaseSettings, SettingsConfigDict


def _csv(value: str) -> list[str]:
    return [item.strip() for item in value.split(",") if item.strip()]


class Settings(BaseSettings):
    model_config = SettingsConfigDict(env_prefix="WITTYCHAT_", env_file=".env", extra="ignore")

    database_url: str = "postgresql+asyncpg://wittychat:wittychat@localhost:5432/wittychat"
    public_url: str = "http://localhost:8000"
    secret_key: str = Field(min_length=32)
    session_ttl_days: int = 30

    oidc_issuer: str | None = None
    oidc_client_id: str | None = None
    oidc_client_secret: str | None = None
    oidc_scopes: str = "openid profile email groups"
    oidc_provider_name: str = "pocketid"
    oidc_admin_groups: str = ""
    oidc_allowed_groups: str = ""

    pocketid_api_key: str | None = None
    directory_sync_interval: int = 600

    dev_login: bool = False

    storage_backend: Literal["local", "s3"] = "local"
    storage_local_path: Path = Path("data/files")
    s3_bucket: str | None = None
    s3_endpoint_url: str | None = None
    s3_region: str | None = None
    s3_access_key: str | None = None
    s3_secret_key: str | None = None
    s3_prefix: str = "wittychat/"
    s3_presign_ttl: int = 3600
    max_upload_mb: int = 100

    broadcast: Literal["auto", "local", "postgres"] = "auto"

    @property
    def oidc_enabled(self) -> bool:
        return bool(self.oidc_issuer and self.oidc_client_id)

    @property
    def admin_groups(self) -> list[str]:
        return _csv(self.oidc_admin_groups)

    @property
    def allowed_groups(self) -> list[str]:
        return _csv(self.oidc_allowed_groups)

    @property
    def is_postgres(self) -> bool:
        return self.database_url.startswith("postgresql")

    @property
    def use_pg_broadcast(self) -> bool:
        return self.broadcast == "postgres" or (self.broadcast == "auto" and self.is_postgres)


@lru_cache
def get_settings() -> Settings:
    return Settings()
