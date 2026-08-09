#!/usr/bin/env python3
import os
import pathlib
import subprocess
import tempfile
import yaml

root = pathlib.Path(__file__).resolve().parents[1]
with tempfile.TemporaryDirectory() as tmp:
    product_name = 'Café 🚀 𐐷 "Widget"\nOperations\x7f\x81\x85\u2028\u2029'
    env = os.environ | {
        "ANS_PRODUCT_NAME": product_name,
        "ANS_OWNER": "acme",
        "ANS_REPO": "widget",
        "ANS_MARKER": "acme-feedback",
        "ANS_REVIEWER_LOGIN": "dev",
        "ANS_BOT_LOGIN": "acme-bot[bot]",
        "ANS_SUPPORT_ADDRESS": "support+agent@acme.test",
        "ANS_FIX_GATE_MODE": "hitl",
        "ANS_APPROVER_ADDRESS": "owner@acme.test",
        "ANS_VERIFY_SETUP_SCRIPT": r"scripts\verify.sh",
    }
    subprocess.run(["bash", str(root / "agentify-render.sh"), "--to", tmp], env=env, check=True)
    config = yaml.safe_load(pathlib.Path(tmp, "autonomous-repo.config.yml").read_text())
    assert config["product_name"] == product_name
    assert config["verify_setup_script"] == r"scripts\verify.sh"
    assert config["repo"] == "acme/widget"

    bad = env | {"ANS_MARKER": "acme\nINJECTED=value"}
    rejected = subprocess.run(
        ["bash", str(root / "agentify-render.sh"), "--to", tmp, "--force"],
        env=bad,
    )
    assert rejected.returncode == 2
