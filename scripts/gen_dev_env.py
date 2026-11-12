#!/usr/bin/env python3
"""Generate local-only development credentials.

Creates gitignored deploy/dev.env and deploy/garage/garage.local.toml.
Existing values are preserved unless --force regenerates the random secrets.
"""
import argparse
import secrets
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
ENV_PATH = ROOT / "deploy" / "dev.env"
TEMPLATE = ROOT / "deploy" / "garage" / "garage.toml.example"
GARAGE_CONFIG = ROOT / "deploy" / "garage" / "garage.local.toml"


def load_env(path):
    values = {}
    if not path.exists():
        return values
    for line in path.read_text(encoding="utf-8").splitlines():
        line = line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        k, v = line.split("=", 1)
        values[k] = v
    return values


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--force", action="store_true", help="regenerate random service secrets")
    args = ap.parse_args()

    env = load_env(ENV_PATH)
    fixed = {
        "DEV_POSTGRES_USER": "wittychat",
        "DEV_POSTGRES_DB": "wittychat",
        "WITTYCHAT_DEV_ADMIN_EMAIL": "admin@wittyloong.com",
        "WITTYCHAT_DEV_ADMIN_PASSWORD": "adminpass",
        "WITTYCHAT_DEV_ALICE_EMAIL": "alice@wittyloong.com",
        "WITTYCHAT_DEV_ALICE_PASSWORD": "alicepass",
        "WITTYCHAT_DEV_CAROL_EMAIL": "carol@wittyloong.com",
        "WITTYCHAT_DEV_CAROL_PASSWORD": "carolpass",
        "WITTYCHAT_DEV_SMOKE_PASSWORD": "smokepass",
        "WITTYCHAT_DEV_P0X_PASSWORD": "p0x-pass",
        "WITTYCHAT_DEV_IMAP_PASSWORD": "imappass",
        "WITTYCHAT_DEV_OLD_IMAP_PASSWORD": "oldpass",
        "WITTYCHAT_S3_ENDPOINT": "http://localhost:3900",
        "WITTYCHAT_S3_BUCKET": "wittychat",
        "WITTYCHAT_S3_REGION": "us-east-1",
        "WITTYCHAT_ONLYOFFICE_URL": "http://localhost:8180",
        "WITTYCHAT_ONLYOFFICE_AUTODETECT": "0",
        "AWS_DEFAULT_REGION": "us-east-1",
    }
    generated = {
        "DEV_POSTGRES_PASSWORD": lambda: secrets.token_urlsafe(24),
        "WITTYCHAT_GARAGE_ACCESS_KEY": lambda: "GK" + secrets.token_hex(10).upper(),
        "WITTYCHAT_GARAGE_SECRET_KEY": lambda: secrets.token_urlsafe(40),
        "WITTYCHAT_GARAGE_RPC_SECRET": lambda: secrets.token_hex(32),
        "WITTYCHAT_GARAGE_ADMIN_TOKEN": lambda: secrets.token_urlsafe(32),
        "WITTYCHAT_ONLYOFFICE_JWT_SECRET": lambda: secrets.token_hex(32),
    }
    for key, value in fixed.items():
        env.setdefault(key, value)
    for key, make in generated.items():
        if args.force or not env.get(key):
            env[key] = make()

    env["WITTYCHAT_DB_URL"] = (
        f"postgres://{env['DEV_POSTGRES_USER']}:{env['DEV_POSTGRES_PASSWORD']}"
        f"@localhost:5432/{env['DEV_POSTGRES_DB']}?sslmode=disable"
    )
    env["WITTYCHAT_TEST_DB_URL"] = (
        f"postgres://{env['DEV_POSTGRES_USER']}:{env['DEV_POSTGRES_PASSWORD']}"
        "@localhost:5432/wittychat_test?sslmode=disable"
    )
    env["ONLYOFFICE_JWT_SECRET"] = env["WITTYCHAT_ONLYOFFICE_JWT_SECRET"]
    env["WITTYCHAT_S3_ACCESS_KEY"] = env["WITTYCHAT_GARAGE_ACCESS_KEY"]
    env["WITTYCHAT_S3_SECRET_KEY"] = env["WITTYCHAT_GARAGE_SECRET_KEY"]
    env["AWS_ACCESS_KEY_ID"] = env["WITTYCHAT_GARAGE_ACCESS_KEY"]
    env["AWS_SECRET_ACCESS_KEY"] = env["WITTYCHAT_GARAGE_SECRET_KEY"]

    order = [
        "DEV_POSTGRES_USER", "DEV_POSTGRES_PASSWORD", "DEV_POSTGRES_DB",
        "WITTYCHAT_DB_URL", "WITTYCHAT_TEST_DB_URL",
        "WITTYCHAT_DEV_ADMIN_EMAIL", "WITTYCHAT_DEV_ADMIN_PASSWORD",
        "WITTYCHAT_DEV_ALICE_EMAIL", "WITTYCHAT_DEV_ALICE_PASSWORD",
        "WITTYCHAT_DEV_CAROL_EMAIL", "WITTYCHAT_DEV_CAROL_PASSWORD",
        "WITTYCHAT_DEV_SMOKE_PASSWORD", "WITTYCHAT_DEV_P0X_PASSWORD",
        "WITTYCHAT_DEV_IMAP_PASSWORD", "WITTYCHAT_DEV_OLD_IMAP_PASSWORD",
        "WITTYCHAT_S3_ENDPOINT", "WITTYCHAT_S3_BUCKET", "WITTYCHAT_S3_REGION",
        "WITTYCHAT_S3_ACCESS_KEY", "WITTYCHAT_S3_SECRET_KEY",
        "WITTYCHAT_GARAGE_ACCESS_KEY", "WITTYCHAT_GARAGE_SECRET_KEY",
        "WITTYCHAT_GARAGE_RPC_SECRET", "WITTYCHAT_GARAGE_ADMIN_TOKEN",
        "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_DEFAULT_REGION",
        "WITTYCHAT_ONLYOFFICE_URL", "ONLYOFFICE_JWT_SECRET",
        "WITTYCHAT_ONLYOFFICE_JWT_SECRET", "WITTYCHAT_ONLYOFFICE_AUTODETECT",
    ]
    lines = [
        "# Generated local development environment — DO NOT COMMIT.",
        "# Service secrets are random; app fixture passwords are intentionally weak.",
    ]
    seen = set()
    for key in order:
        lines.append(f"{key}={env[key]}")
        seen.add(key)
    for key in sorted(env):
        if key not in seen:
            lines.append(f"{key}={env[key]}")
    ENV_PATH.write_text("\n".join(lines) + "\n", encoding="utf-8")
    ENV_PATH.chmod(0o600)

    rendered = (TEMPLATE.read_text(encoding="utf-8")
                .replace("__GARAGE_RPC_SECRET__", env["WITTYCHAT_GARAGE_RPC_SECRET"])
                .replace("__GARAGE_ADMIN_TOKEN__", env["WITTYCHAT_GARAGE_ADMIN_TOKEN"]))
    GARAGE_CONFIG.write_text(rendered, encoding="utf-8")
    GARAGE_CONFIG.chmod(0o600)

    print(f"wrote {ENV_PATH.relative_to(ROOT)}")
    print(f"wrote {GARAGE_CONFIG.relative_to(ROOT)}")
    print("Git Bash:  set -a; . deploy/dev.env; set +a")
    print("Compose:   docker compose --env-file deploy/dev.env -f deploy/docker-compose.dev.yml up -d")


if __name__ == "__main__":
    main()
