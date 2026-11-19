# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import os
from pathlib import Path
import subprocess
import unittest

CHART = Path(__file__).resolve().parents[1]


class DesktopIngressTests(unittest.TestCase):
    def test_mtls_authority_overrides_only_listener_bindings(self):
        source = (CHART / "configs/desktop-entrypoint.sh").read_text()
        bindings = source[source.index("DISPLAY_NUM="):source.index("# Resolve log destinations")]
        output = '\nprintf "%s\\n" "$VNC_PORT" "$NOVNC_PORT" "$NOVNC_LISTEN" "$VNC_BIND_ARGS" "$CHROME_BIND_ARGS" "$SCREEN_RES"\n'
        for enabled, expected in (
            ("false", ["5901", "6081", "6081", "", "", "1280x720x24"]),
            ("true", ["5900", "6080", "127.0.0.1:6080", "-listen 127.0.0.1 -localhost",
                      "--remote-debugging-address=127.0.0.1", "1280x720x24"]),
        ):
            with self.subTest(mtls=enabled):
                result = subprocess.run(["bash", "-eu", "-c", bindings + output],
                                        env=dict(os.environ, CETIRUN_INGRESS_MTLS=enabled,
                                                 VNC_PORT="5901", NOVNC_PORT="6081", SCREEN_RESOLUTION="1280x720x24"),
                                        text=True, capture_output=True, check=True)
                self.assertEqual(result.stdout.splitlines(), expected)

    def test_managed_startup_and_probe_are_chart_owned(self):
        template = (CHART / "templates/_platform.tpl").read_text()
        config = (CHART / "templates/configmap.yaml").read_text()
        source = (CHART / "configs/desktop-entrypoint.sh").read_text()
        self.assertIn('command: ["/bin/bash", "/etc/cetirun/desktop-ingress-entrypoint.sh"]', template)
        self.assertIn('command: ["python3", "/etc/cetirun/desktop-ingress-probe.py"]', template)
        self.assertIn('configs/desktop-ingress-probe.py', config)
        self.assertIn('${VNC_BIND_ARGS} -rfbport ${VNC_PORT}', source)
        self.assertIn('${NOVNC_LISTEN} localhost:${VNC_PORT}', source)
        self.assertIn('--remote-debugging-port=18800 ${CHROME_BIND_ARGS}', source)


if __name__ == "__main__":
    unittest.main()
