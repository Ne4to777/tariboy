"""Helpers the fixture scripts share. Not executable: scripts run it with python3.

  python3 snapshot.py field visit.id     a field of the task snapshot
  python3 snapshot.py artifact NAME      the current value of an artifact, or ""
  python3 snapshot.py sha256 VARIABLE    SHA-256 of an environment variable
"""
import hashlib
import json
import os
import sys


def snapshot():
    with open(os.environ["TARIBOY_TASK_FILE"], encoding="utf-8") as f:
        return json.load(f)


def main(argv):
    command, arg = argv[1], argv[2]
    if command == "field":
        value = snapshot()
        for part in arg.split("."):
            value = value[part]
        print(value)
    elif command == "artifact":
        values = {a["name"]: a["value"] for a in snapshot()["artifacts"]}
        print(values.get(arg, ""))
    elif command == "sha256":
        print(hashlib.sha256(os.environ.get(arg, "").encode()).hexdigest())
    else:
        sys.exit("unknown command " + command)


main(sys.argv)
