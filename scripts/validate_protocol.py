"""Validate the protocol and its examples without a live remote service."""
from pathlib import Path
import copy
import json
import re
import yaml
from jsonschema import Draft202012Validator, FormatChecker
from openapi_spec_validator import validate

spec = yaml.safe_load(Path("api/provider.openapi.yaml").read_text())
validate(spec)


def check(name, value, valid=True):
    schema = {"$ref": f"#/components/schemas/{name}", "components": spec["components"]}
    errors = list(Draft202012Validator(schema, format_checker=FormatChecker()).iter_errors(value))
    assert bool(errors) != valid, (name, [error.message for error in errors])


assert set(spec["paths"]) == {"/v1/submit", "/v1/launches"}
assert spec["info"]["version"] == "1.0.0"
for operations in spec["paths"].values():
    for op in operations.values():
        bodies = [op["requestBody"]] if "requestBody" in op else []
        bodies.extend(op["responses"].values())
        for body in bodies:
            media = body.get("content", {}).get("application/json", {})
            if "example" in media:
                check(media["schema"]["$ref"].split("/")[-1], media["example"])

# Human-readable examples are executable contract fixtures, too.
doc = Path("REMOTE_PROVIDER_PROTOCOL.md").read_text()
examples = re.findall(r"<!-- schema: (\w+) -->\s*```json\n(.*?)\n```", doc, re.DOTALL)
assert len(examples) == doc.count("```json") == 4
for name, example in examples:
    check(name, json.loads(example))

request = spec["paths"]["/v1/submit"]["post"]["requestBody"]["content"]["application/json"]["example"]
check("SubmitRequest", {"items": []}, False)
check("SubmitRequest", {"items": [request["items"][0]] * 101}, False)
for field, value in (("cpu_millis", 0), ("memory_bytes", 1.5), ("scratch_bytes", 9007199254740992), ("timeout_seconds", 31536001)):
    bad = copy.deepcopy(request)
    bad["items"][0][field] = value
    check("SubmitRequest", bad, False)
for field in ("process", "metadata", "image", "cpu_millis"):
    bad = copy.deepcopy(request)
    del bad["items"][0][field]
    check("SubmitRequest", bad, False)
bad = copy.deepcopy(request)
bad["items"][0]["plan_token"] = "obsolete"
check("SubmitRequest", bad, False)
check("SubmissionResult", {"launch_id": "test", "status": "accepted"})
check("SubmissionResult", {"status": "accepted"}, False)
for status in ("no_capacity", "unsupported", "unavailable", "rejected", "unknown"):
    check("SubmissionResult", {"launch_id": "test", "status": status}, False)
for name in ("AcceptedResult", "UnknownResult", "DeclinedResult"):
    assert "refs" not in spec["components"]["schemas"][name]["properties"]
check("ListResponse", {"items": []})
check("ListResponse", {"items": [{"id":"native", "launch_id":"test", "state":"succeeded", "metadata":{"key":"value"}, "refs":[]}]}, False)
check("SubmissionResult", {"launch_id": "test", "status": "unknown", "reason": "Connection lost."})
check("SubmissionResult", {"launch_id": "test", "status": "prepared"}, False)
print("OpenAPI schema, specification/documentation examples, and negative cases passed.")
