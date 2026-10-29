from __future__ import annotations

import os
import shutil
from abc import ABC, abstractmethod
from pathlib import Path
from urllib.parse import quote

import anyio
from starlette.responses import FileResponse, RedirectResponse, Response

from wittychat_server.config import Settings

# Only these render inline; everything else is forced to download.
INLINE_TYPES = {"image/png", "image/jpeg", "image/gif", "image/webp"}
SAFE_HEADERS = {"X-Content-Type-Options": "nosniff", "Content-Security-Policy": "sandbox"}


def content_disposition(filename: str, inline: bool) -> str:
    return f"{'inline' if inline else 'attachment'}; filename*=UTF-8''{quote(filename)}"


class Storage(ABC):
    @staticmethod
    def key_for(sha256: str) -> str:
        return f"blobs/{sha256[:2]}/{sha256[2:4]}/{sha256}"

    @abstractmethod
    async def put(self, key: str, source: Path, content_type: str) -> None: ...

    @abstractmethod
    async def response(self, key: str, filename: str, content_type: str, *, download: bool) -> Response: ...

    @abstractmethod
    def describe(self) -> dict[str, str]: ...


class LocalStorage(Storage):
    def __init__(self, root: Path) -> None:
        self.root = root.resolve()

    def _path(self, key: str) -> Path:
        path = (self.root / key).resolve()
        if not path.is_relative_to(self.root):
            raise ValueError("invalid storage key")
        return path

    async def put(self, key: str, source: Path, content_type: str) -> None:
        target = self._path(key)

        def _store() -> None:
            target.parent.mkdir(parents=True, exist_ok=True)
            tmp = target.with_suffix(".part")
            shutil.copyfile(source, tmp)
            os.replace(tmp, target)

        await anyio.to_thread.run_sync(_store)

    async def response(self, key: str, filename: str, content_type: str, *, download: bool) -> Response:
        inline = not download and content_type in INLINE_TYPES
        return FileResponse(
            self._path(key),
            media_type=content_type if inline else "application/octet-stream",
            headers={**SAFE_HEADERS, "Content-Disposition": content_disposition(filename, inline)},
        )

    def describe(self) -> dict[str, str]:
        return {"backend": "local", "path": str(self.root)}


class S3Storage(Storage):
    def __init__(self, settings: Settings) -> None:
        import boto3

        if not settings.s3_bucket:
            raise ValueError("WITTYCHAT_S3_BUCKET is required for the s3 storage backend")
        self.settings = settings
        self.bucket = settings.s3_bucket
        self.prefix = settings.s3_prefix
        self.client = boto3.client(
            "s3",
            endpoint_url=settings.s3_endpoint_url,
            region_name=settings.s3_region,
            aws_access_key_id=settings.s3_access_key,
            aws_secret_access_key=settings.s3_secret_key,
        )

    async def put(self, key: str, source: Path, content_type: str) -> None:
        await anyio.to_thread.run_sync(
            lambda: self.client.upload_file(
                str(source), self.bucket, self.prefix + key, ExtraArgs={"ContentType": content_type}
            )
        )

    async def response(self, key: str, filename: str, content_type: str, *, download: bool) -> Response:
        inline = not download and content_type in INLINE_TYPES
        url = self.client.generate_presigned_url(
            "get_object",
            Params={
                "Bucket": self.bucket,
                "Key": self.prefix + key,
                "ResponseContentDisposition": content_disposition(filename, inline),
                "ResponseContentType": content_type if inline else "application/octet-stream",
            },
            ExpiresIn=self.settings.s3_presign_ttl,
        )
        return RedirectResponse(url, status_code=302)

    def describe(self) -> dict[str, str]:
        return {
            "backend": "s3",
            "bucket": self.bucket,
            "endpoint": self.settings.s3_endpoint_url or "AWS default",
            "prefix": self.prefix,
        }


def make_storage(settings: Settings) -> Storage:
    if settings.storage_backend == "s3":
        return S3Storage(settings)
    return LocalStorage(settings.storage_local_path)
