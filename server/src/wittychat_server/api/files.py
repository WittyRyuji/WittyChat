from __future__ import annotations

import hashlib
import mimetypes
import os
import tempfile
from pathlib import Path

from fastapi import APIRouter, HTTPException, Query, Request, UploadFile
from fastapi.responses import Response
from sqlalchemy.dialects.postgresql import insert as pg_insert
from sqlalchemy.dialects.sqlite import insert as sqlite_insert

from wittychat_server import chat, models
from wittychat_server.deps import DB, AppSettings, CurrentUser
from wittychat_server.serializers import attachment_out
from wittychat_shared import protocol as p

router = APIRouter(prefix="/files", tags=["files"])
CHUNK = 1024 * 1024


def _clean_filename(name: str | None) -> str:
    base = os.path.basename((name or "").replace("\\", "/")).strip()
    return base[:255] or "file"


@router.post("", response_model=p.Attachment, status_code=201)
async def upload(file: UploadFile, request: Request, db: DB, user: CurrentUser, settings: AppSettings) -> p.Attachment:
    storage = request.app.state.storage
    limit = settings.max_upload_mb * 1024 * 1024
    filename = _clean_filename(file.filename)
    content_type = file.content_type or mimetypes.guess_type(filename)[0] or "application/octet-stream"
    digest, size = hashlib.sha256(), 0
    fd, tmp_name = tempfile.mkstemp(prefix="wittychat-upload-")
    tmp = Path(tmp_name)
    try:
        with os.fdopen(fd, "wb") as out:
            while chunk := await file.read(CHUNK):
                size += len(chunk)
                if size > limit:
                    raise HTTPException(413, f"file exceeds {settings.max_upload_mb} MB")
                digest.update(chunk)
                out.write(chunk)
        sha = digest.hexdigest()
        if await db.get(models.Blob, sha) is None:
            key = storage.key_for(sha)
            await storage.put(key, tmp, content_type)
            insert = pg_insert if db.get_bind().dialect.name == "postgresql" else sqlite_insert
            await db.execute(
                insert(models.Blob).values(sha256=sha, size=size, storage_key=key).on_conflict_do_nothing()
            )
    finally:
        tmp.unlink(missing_ok=True)
    attachment = models.Attachment(
        blob_sha256=sha, filename=filename, content_type=content_type[:255], size=size, uploader_id=user.id
    )
    db.add(attachment)
    await db.commit()
    return attachment_out(attachment)


@router.get("/{attachment_id}")
async def download(
    attachment_id: int, request: Request, db: DB, user: CurrentUser, download: bool = Query(False)
) -> Response:
    attachment = await db.get(models.Attachment, attachment_id)
    allowed = False
    if attachment is not None:
        if attachment.message_id is None:
            allowed = attachment.uploader_id == user.id
        else:
            message = await db.get(models.Message, attachment.message_id)
            if message is not None and message.deleted_at is None:
                allowed = await chat.can_read(db, await chat.get_channel(db, message.channel_id), user)
    if not allowed:
        raise HTTPException(404, "file not found")
    blob = await db.get_one(models.Blob, attachment.blob_sha256)
    return await request.app.state.storage.response(
        blob.storage_key, attachment.filename, attachment.content_type, download=download
    )
