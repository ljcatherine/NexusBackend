import json
import re
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
POLICY_PATH = ROOT / "docs" / "tailscale-acl.example.hujson"
PS5_TAG = "tag:nexus-ps5"
GATEWAY_TAG = "tag:nexus-gateway"
PS5_PORTS = {"8081", "8082", "8083"}


def strip_hujson(text):
    """Remove // comments and trailing commas outside of JSON strings."""
    out = []
    in_string = False
    i = 0
    while i < len(text):
        ch = text[i]
        if in_string:
            out.append(ch)
            if ch == "\\":
                out.append(text[i + 1])
                i += 2
                continue
            if ch == '"':
                in_string = False
        elif ch == '"':
            in_string = True
            out.append(ch)
        elif text.startswith("//", i):
            while i < len(text) and text[i] != "\n":
                i += 1
            continue
        else:
            out.append(ch)
        i += 1
    cleaned = "".join(out)
    # shortcut: regex trailing-comma removal ignores strings, fine while no
    # string value ends with a comma before a closing bracket.
    return re.sub(r",(\s*[\]}])", r"\1", cleaned)


def load_policy():
    return json.loads(strip_hujson(POLICY_PATH.read_text(encoding="utf-8")))


class TailscaleAclTests(unittest.TestCase):
    def setUp(self):
        self.policy = load_policy()
        self.rules = self.policy["acls"]

    def ps5_rules(self):
        return [
            rule
            for rule in self.rules
            if any(dst.rpartition(":")[0] == PS5_TAG for dst in rule.get("dst", []))
        ]

    def test_only_one_rule_reaches_ps5(self):
        self.assertEqual(len(self.ps5_rules()), 1)

    def test_ps5_rule_source_is_exactly_gateway_tag(self):
        self.assertEqual(self.ps5_rules()[0]["src"], [GATEWAY_TAG])

    def test_ps5_rule_ports_are_exactly_8081_8082_8083(self):
        dst = self.ps5_rules()[0]["dst"]
        self.assertEqual(len(dst), 1)
        host, _, ports = dst[0].rpartition(":")
        self.assertEqual(host, PS5_TAG)
        self.assertEqual(set(ports.split(",")), PS5_PORTS)
        self.assertEqual(len(ports.split(",")), len(PS5_PORTS))

    def test_no_rule_uses_wildcard_destination(self):
        for rule in self.rules:
            with self.subTest(rule=rule):
                self.assertNotIn("*", rule.get("dst", []))


if __name__ == "__main__":
    unittest.main()
