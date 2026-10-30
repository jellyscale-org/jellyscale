#!/usr/bin/env python3
"""List routes hidden from the OpenAPI spec by ApiExplorerSettings(IgnoreApi).

Usage: extract.py <jellyfin_source_dir>

A method is hidden when it, or its whole controller, has IgnoreApi = true. Each
controller uses one class-level route prefix, which is enough for this source.
"""
import re
import sys
from pathlib import Path

HIDE = re.compile(r"IgnoreApi\s*=\s*true")
VERB = re.compile(r'\[Http(Get|Post|Put|Delete|Head|Patch)(?:\("([^"]*)"[^\]]*\)|[^\]]*)\]')
CLASS_ROUTE = re.compile(r'\[Route\("([^"]*)"\)\]')
CLASS_DECL = re.compile(r"\bclass\s+(\w+)")
METHOD_END = re.compile(r"^\s*public\s.*\(")


def scan(path):
    text = path.read_text(encoding="utf-8-sig")
    decl = CLASS_DECL.search(text)
    if not decl:
        return
    head = text[: decl.start()]
    # Attributes in the file header, including Route, belong to the class.
    # Controllers without one inherit [Route("[controller]")] from the base class.
    # An explicit [Route("")] is different: it means root-level routes.
    declared = CLASS_ROUTE.findall(head)
    prefix = declared[0] if declared else "[controller]"
    class_hidden = bool(HIDE.search(head))
    name = decl.group(1).removesuffix("Controller")

    pending, hidden, obsolete = [], False, False
    for line in text[decl.end():].splitlines():
        hidden = hidden or bool(HIDE.search(line))
        obsolete = obsolete or "[Obsolete" in line
        for verb, route in VERB.findall(line):
            pending.append((verb.upper(), route))
        if METHOD_END.match(line):
            if pending and (hidden or class_hidden):
                for verb, route in pending:
                    # A leading / or ~/ makes the route absolute, ignoring the prefix.
                    if route.startswith(("/", "~/")):
                        full = route.lstrip("~/")
                    else:
                        full = "/".join(p for p in (prefix, route) if p).replace("[controller]", name)
                    yield name, verb, "/" + full, "obsolete" if obsolete else "current"
            pending, hidden, obsolete = [], False, False


def main():
    routes = []
    for path in sorted(Path(sys.argv[1], "Jellyfin.Api", "Controllers").glob("*.cs")):
        routes.extend(scan(path))
    for name, verb, route, state in sorted(set(routes)):
        print(f"{state:<9} {name:<24} {verb:<7} {route}")
    print(f"\n{len(set(routes))} hidden routes")


if __name__ == "__main__":
    main()
