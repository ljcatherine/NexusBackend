import re
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
SYSTEM_UNIT = (ROOT / "gateway" / "nexus-gateway.service").read_text()
USER_UNIT = (ROOT / "gateway" / "nexus-gateway-user.service").read_text()
ENV_EXAMPLE = (ROOT / "gateway" / ".env.example").read_text()

LOAD_CREDENTIAL = "LoadCredential=nexus-ps5-token:"
OPAQUE_VALUE = re.compile(r"[A-Za-z0-9+/_=-]{32,}")


class GatewayUnitTests(unittest.TestCase):
    def test_system_unit_loads_ps5_token_from_etc(self):
        self.assertIn(LOAD_CREDENTIAL + "/etc/nexus-gateway/ps5-token", SYSTEM_UNIT.splitlines())

    def test_user_unit_loads_ps5_token_from_home_config(self):
        self.assertIn(LOAD_CREDENTIAL + "%h/.config/nexus-gateway/ps5-token", USER_UNIT.splitlines())

    def test_env_example_names_credential_without_token_value(self):
        lines = ENV_EXAMPLE.splitlines()
        self.assertIn("NEXUS_PS5_TOKEN_CREDENTIAL=nexus-ps5-token", lines)
        credential_lines = [line for line in lines if "PS5_TOKEN" in line and not line.startswith("#")]
        self.assertTrue(credential_lines)
        for line in credential_lines:
            with self.subTest(line=line):
                value = line.split("=", 1)[1]
                self.assertFalse(OPAQUE_VALUE.search(value), "credential line holds a token-like value")


if __name__ == "__main__":
    unittest.main()
