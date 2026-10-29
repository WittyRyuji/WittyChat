from fastapi import APIRouter

from wittychat_server.api import auth, channels, files, messages, sync, users

router = APIRouter(prefix="/api")
for module in (auth, sync, users, channels, messages, files):
    router.include_router(module.router)
