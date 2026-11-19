# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Opt-in, network-free container checks of the rendered native Memory mounts."""

import os
from pathlib import Path
import subprocess
import tempfile
import unittest

from test_native_gateway_chart import RUNTIME_CHART, object_of_kind, render


@unittest.skipUnless(os.environ.get("CETIRUN_MEMORY_TEST_IMAGE"), "set a cached local image with /bin/sh")
class NativeMemoryMountTests(unittest.TestCase):
    def test_live_share_and_writable_personal_skills(self):
        image = os.environ["CETIRUN_MEMORY_TEST_IMAGE"]
        subprocess.run(["docker", "image", "inspect", image], check=True, capture_output=True)
        for agent_type in ("openclaw", "hermes"):
            for shared_path in ("/home/vibe/.aks/skills", "/home/vibe/.agents/skills"):
                with self.subTest(agent_type=agent_type, shared_path=shared_path), tempfile.TemporaryDirectory(prefix="memory-mounts-") as temp:
                    root = Path(temp)
                    sources = {name: root / name for name in ("data", "tmp", "config", "shared-skills", "context-view", "memory-assets")}
                    for directory in sources.values():
                        directory.mkdir(mode=0o777)
                        directory.chmod(0o777)
                    personal = sources["data"] / ".agents"
                    (personal / "skills/personal").mkdir(parents=True)
                    (personal / "private.txt").write_text("personal content")
                    (personal / "skills/personal/SKILL.md").write_text("personal skill")
                    personal.chmod(0o777)
                    (personal / "skills").chmod(0o777)
                    (sources["data"] / ".aks/skills").mkdir(parents=True)
                    (personal / "AGENTS.md").touch()
                    context = sources["context-view"] / "current"
                    (context / "skills/cetirun-memory").mkdir(parents=True)
                    (context / "AGENTS.md").write_text("platform instructions")
                    (context / "skills/cetirun-memory/SKILL.md").write_text("platform memory")
                    (sources["memory-assets"] / "skills/cetirun-memory").mkdir(parents=True)
                    (sources["memory-assets"] / "skills/cetirun-memory/SKILL.md").write_text("platform memory")
                    shared = sources["shared-skills"]
                    (shared / "team-skill").mkdir()
                    skill = shared / "team-skill/SKILL.md"
                    skill.write_text("version one")
                    values = {
                        "agentType": agent_type,
                        "memoryContext": {"image": "registry.example.com/memory:1.2.3"},
                        "desktop": {"enabled": False},
                        "skills": {"shared": True, "mountPath": shared_path, "resourceGroup": "test-group", "storageAccount": "testskills", "containerName": "approved-skills"},
                        "aiGateway": {"enabled": True, "keySecretName": "synthetic-model-key"},
                        "secrets": {"provider": "openai", "model": "example-model", "existingSecret": "synthetic-agent-secret", "apiBase": "http://gateway.example.com/v1"},
                    }
                    objects = render(RUNTIME_CHART, values)
                    mounts = object_of_kind(objects, "StatefulSet")["spec"]["template"]["spec"]["containers"][0]["volumeMounts"]
                    command = ["docker", "run", "--detach", "--rm", "--pull=never", "--network=none", "--read-only", "--user=1024:1024", "--cap-drop=ALL", "--security-opt=no-new-privileges", "--entrypoint=/bin/sh"]
                    for mount in mounts:
                        source = sources[mount["name"]] / mount.get("subPath", "")
                        command += ["--mount", f"type=bind,src={source},dst={mount['mountPath']}" + (",readonly" if mount.get("readOnly") else "")]
                    started = subprocess.run(command + [image, "-c", "sleep 120"], text=True, capture_output=True, timeout=30)
                    self.assertEqual(started.returncode, 0, started.stderr)
                    container = started.stdout.strip()
                    self.addCleanup(subprocess.run, ["docker", "rm", "--force", container], capture_output=True, timeout=30)

                    def run(script):
                        result = subprocess.run(["docker", "exec", container, "/bin/sh", "-ec", script], text=True, capture_output=True, timeout=20)
                        self.assertEqual(result.returncode, 0, result.stderr)
                        return result.stdout.strip()

                    self.assertEqual(run("cat /home/vibe/.agents/private.txt"), "personal content")
                    run("printf retained > /home/vibe/.agents/new.txt")
                    if shared_path != "/home/vibe/.agents/skills":
                        self.assertEqual(run("cat /home/vibe/.agents/skills/personal/SKILL.md"), "personal skill")
                        run("mkdir /home/vibe/.agents/skills/new-personal")
                    self.assertEqual(run("cat /opt/cetirun-memory/skills/cetirun-memory/SKILL.md"), "platform memory")
                    self.assertEqual(run(f"cat {shared_path}/team-skill/SKILL.md"), "version one")
                    skill.write_text("version two")
                    (shared / "published-later").mkdir()
                    (shared / "published-later/SKILL.md").write_text("new skill")
                    self.assertEqual(run(f"cat {shared_path}/team-skill/SKILL.md"), "version two")
                    self.assertEqual(run(f"cat {shared_path}/published-later/SKILL.md"), "new skill")
                    run(f"! touch {shared_path}/write-denied; ! touch /opt/cetirun-memory/skills/cetirun-memory/write-denied")
                    subprocess.run(["docker", "rm", "--force", container], check=True, capture_output=True, timeout=30)


if __name__ == "__main__":
    unittest.main()
