"""Validate the protocol and its examples without a live remote service."""
from pathlib import Path
import copy
import yaml
from jsonschema import Draft202012Validator, FormatChecker
from openapi_spec_validator import validate

spec = yaml.safe_load(Path("api/provider.openapi.yaml").read_text())
validate(spec)

def check(name, value, valid=True):
    schema = {"$ref": f"#/components/schemas/{name}", "components": spec["components"]}
    errors = list(Draft202012Validator(schema, format_checker=FormatChecker()).iter_errors(value))
    assert bool(errors) != valid, (name, [error.message for error in errors])

for path in ("/v1/prepare", "/v1/submit"):
    op = spec["paths"][path]["post"]
    for media in (op["requestBody"]["content"]["application/json"], op["responses"]["200"]["content"]["application/json"]):
        check(media["schema"]["$ref"].split("/")[-1], media["example"])

request = spec["paths"]["/v1/prepare"]["post"]["requestBody"]["content"]["application/json"]["example"]
check("PrepareRequest", {"items": []}, False)
check("PrepareRequest", {"items": [request["items"][0]] * 101}, False)
for field, value in (("cpu_millis", 0), ("memory_bytes", 1.5), ("scratch_bytes", 9007199254740992)):
    bad = copy.deepcopy(request)
    bad["items"][0][field] = value
    check("PrepareRequest", bad, False)
check("SubmissionResult", {"launch_id": "test", "status": "accepted", "refs": []}, False)
check("SubmissionResult", {"launch_id": "test", "status": "unknown", "reason": "Connection lost."})
check("PreparationResult", {"launch_id": "test", "status": "prepared"}, False)
print("OpenAPI schema, examples, and negative cases passed.")
