#!/usr/bin/env python3
"""Check out or verify the pinned deep runtime Git baseline."""

import argparse
import json
import os
from pathlib import Path
import re
import subprocess
import sys
from urllib.parse import urlsplit


DEFAULT_LOCK = Path(__file__).resolve().parents[2] / "docs/deep-runtime/baseline-lock.json"
SHA = re.compile(r"[0-9a-f]{40}")
PLATFORM = "platform-smolvm"
RUNTIME = "smolvm"
DEPENDENCIES = ("libkrun", "libkrunfw")
GIT_ENV = os.environ.copy()
for key in (
    "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_COMMON_DIR", "GIT_CONFIG", "GIT_CONFIG_COUNT",
    "GIT_CONFIG_PARAMETERS", "GIT_DIR", "GIT_INDEX_FILE", "GIT_NAMESPACE",
    "GIT_OBJECT_DIRECTORY", "GIT_QUARANTINE_PATH", "GIT_WORK_TREE",
):
    GIT_ENV.pop(key, None)
GIT_ENV.update(GIT_TERMINAL_PROMPT="0", GIT_NO_REPLACE_OBJECTS="1", GIT_OPTIONAL_LOCKS="0",
               GIT_LFS_SKIP_SMUDGE="1")


def git(repo, *args, protocol=None, allowed=(0,)):
    command = ["git", "-c", "core.hooksPath=/dev/null", "-c", "protocol.allow=never"]
    if protocol:
        command.extend(["-c", f"protocol.{protocol}.allow=always"])
    result = subprocess.run(command + list(args), cwd=repo, env=GIT_ENV, check=False,
                            text=True, capture_output=True)
    if result.returncode not in allowed:
        raise ValueError(f"{repo}: git {args[0]} failed: {result.stderr.strip()}")
    return result.stdout.rstrip("\n")


def github_repository(url):
    # GitHub Git URLs may omit the .git suffix. Baseline submodule URLs may use SSH.
    if url.startswith("git@github.com:"):
        url = "ssh://git@github.com/" + url.removeprefix("git@github.com:")
    parsed = urlsplit(url)
    path = parsed.path.rstrip("/").removesuffix(".git").lstrip("/")
    if parsed.netloc not in ("github.com", "git@github.com") or parsed.scheme not in ("https", "ssh"):
        raise ValueError(f"expected a GitHub repository URL, got {url!r}")
    if parsed.query or parsed.fragment or not re.fullmatch(r"[\w.-]+/[\w.-]+", path, re.ASCII):
        raise ValueError(f"invalid GitHub repository URL {url!r}")
    return path.lower()


def locked_https(url):
    github_repository(url)
    if not url.startswith("https://github.com/"):
        raise ValueError(f"lock requires an HTTPS repository URL, got {url!r}")
    return url


def locked_sha(value):
    if not isinstance(value, str) or not SHA.fullmatch(value):
        raise ValueError(f"expected a full Git object ID, got {value!r}")
    return value


def load_lock(path):
    lock = json.loads(path.read_text())
    if lock["schema"] != "engineering-baseline/v1":
        raise ValueError("expected engineering-baseline/v1")

    platform = lock["platform"]
    runtime = lock["smolvm"]
    sources = {
        PLATFORM: {"origin": locked_https(platform["repository"]),
                   "commit": locked_sha(platform["base_commit"])},
        RUNTIME: {"origin": locked_https(runtime["repository"]),
                  "upstream": locked_https(runtime["upstream"]),
                  "commit": locked_sha(runtime["base_commit"])},
    }
    for name in DEPENDENCIES:
        field = lock["submodules"][name]
        sources[name] = {"origin": locked_https(field["planned_fork"]),
                         "upstream": locked_https(field["upstream"]),
                         "commit": locked_sha(field["commit"])}
    firmware = lock["submodules"]["libkrunfw"]
    sources["libkrunfw"].update(observed=locked_sha(firmware["observed_upstream_main"]),
                               tree=locked_sha(firmware["both_commits_tree"]))
    branch = runtime["development_branch"]
    if branch != "deep-runtime/main" or platform["development_branch"] != branch:
        raise ValueError("expected the deep-runtime/main development branch")
    return sources, branch


def check_repo(repo, source):
    if repo.is_symlink() or Path(git(repo, "rev-parse", "--show-toplevel")).resolve() != repo.resolve():
        raise ValueError(f"expected a worktree at {repo}")
    commit = git(repo, "rev-parse", "--verify", "HEAD^{commit}")
    if commit != source["commit"]:
        raise ValueError(f"{repo}: expected HEAD {source['commit']}, found {commit}")
    for remote in ("origin", "upstream"):
        if remote not in source:
            continue
        actual = git(repo, "remote", "get-url", "--all", remote)
        if github_repository(locked_https(actual)) != github_repository(source[remote]):
            raise ValueError(f"{repo}: unexpected {remote} URL {actual!r}")
    tree = git(repo, "rev-parse", "HEAD^{tree}")
    if "tree" in source:
        observed = git(repo, "rev-parse", "--verify", f"{source['observed']}^{{tree}}")
        if tree != source["tree"] or observed != source["tree"]:
            raise ValueError(f"{repo}: locked and observed firmware commits must share tree {source['tree']}")
    dirty = git(repo, "status", "--porcelain=v1", "--untracked-files=all", "--ignore-submodules=none")
    if dirty:
        raise ValueError(f"{repo}: worktree contains local changes\n{dirty}")
    pointers = git(repo, "grep", "-Ilz", r"^version https://git-lfs\.github\.com/spec/v1$",
                   "HEAD", allowed=(0, 1)).split("\0")
    unresolved = []
    for pointer in filter(None, pointers):
        path = pointer.removeprefix("HEAD:")
        with (repo / path).open("rb") as tracked:
            if tracked.readline(64) == b"version https://git-lfs.github.com/spec/v1\n":
                unresolved.append(path)
    return {"path": str(repo), "commit": commit, "tree": tree, "lfs_pointers_in_worktree": unresolved,
            **{key: source[key] for key in ("origin", "upstream", "observed") if key in source}}


def gitlinks(repo):
    entries = git(repo, "ls-tree", "-rz", "HEAD").split("\0")
    links = {}
    for entry in filter(None, entries):
        metadata, path = entry.split("\t", 1)
        mode, kind, commit = metadata.split()
        if (mode, kind) == ("160000", "commit"):
            links[path] = commit
    return links


def dependencies(repo, sources):
    links = gitlinks(repo)
    records = git(repo, "config", "--blob=HEAD:.gitmodules", "--null", "--get-regexp",
                  r"^submodule\..*\.path$").split("\0")
    configured = {}
    for record in filter(None, records):
        key, path = record.split("\n", 1)
        if path in configured or Path(path).is_absolute() or Path(path).as_posix() != path:
            raise ValueError(f"{repo}: invalid or duplicate submodule path {path!r}")
        if not Path(path).parts or any(part in ("..", ".git") for part in Path(path).parts):
            raise ValueError(f"{repo}: unsafe submodule path {path!r}")
        configured[path] = key.removesuffix(".path")
    if set(configured) != set(links) or sorted(links.values()) != sorted(sources[n]["commit"] for n in DEPENDENCIES):
        raise ValueError(f"{repo}: .gitmodules and gitlinks do not match all locked dependencies: {links}")
    result = {}
    for name in DEPENDENCIES:
        path = next(p for p, sha in links.items() if sha == sources[name]["commit"])
        key = configured[path]
        url = git(repo, "config", "--blob=HEAD:.gitmodules", "--get-all", f"{key}.url")
        if url in (f"../{name}", f"../{name}.git"):
            url = sources[RUNTIME]["upstream"].rsplit("/", 1)[0] + "/" + name
        if github_repository(url) != github_repository(sources[name]["upstream"]):
            raise ValueError(f"{repo}: unexpected upstream URL {url!r} for locked gitlink {path}")
        result[name] = (path, key)
    return result


def verify(root, sources):
    report = {name: check_repo(root / name, sources[name]) for name in (PLATFORM, RUNTIME)}
    links = dependencies(root / RUNTIME, sources)
    for name, (path, _) in links.items():
        report[name] = check_repo(root / RUNTIME / path, sources[name])
        report[name]["gitlink"] = path
    for name in (PLATFORM, *DEPENDENCIES):
        if gitlinks(Path(report[name]["path"])):
            raise ValueError(f"{name}: source baseline has unexpected nested gitlinks")
    return report


def fetch(repo, source, transport, protocol):
    repo.mkdir(parents=True, exist_ok=True)
    if any(repo.iterdir()):
        raise ValueError(f"checkout path is occupied: {repo}")
    git(repo, "init", "--quiet", "--template=")
    for remote in ("origin", "upstream"):
        if remote in source:
            git(repo, "remote", "add", remote, source[remote])
    commits = [source["commit"]]
    if "observed" in source:
        commits.append(source["observed"])
    for index, commit in enumerate(commits):
        git(repo, "fetch", "--no-tags", "--no-recurse-submodules", "--update-shallow", transport,
            f"{commit}:refs/deep-baseline/{'locked' if index == 0 else 'observed'}", protocol=protocol)
    git(repo, "checkout", "--quiet", "--detach", source["commit"])


def checkout(root, sources, mirror_dir, https_source):
    transports = {}
    for name, source in sources.items():
        if mirror_dir is not None:
            transport = mirror_dir / f"{name}.git"
            if not transport.exists():
                raise ValueError(f"local Git mirror or bundle missing: {transport}")
            if transport.resolve().is_relative_to(root):
                raise ValueError(f"mirror must be outside the checkout: {transport}")
            transports[name] = str(transport)
        else:
            transports[name] = source.get("upstream", source["origin"]) if https_source == "upstream" else source["origin"]
    root.mkdir()
    protocol = "file" if mirror_dir is not None else "https"
    for name in (PLATFORM, RUNTIME):
        fetch(root / name, sources[name], transports[name], protocol)
    for name, (path, key) in dependencies(root / RUNTIME, sources).items():
        fetch(root / RUNTIME / path, sources[name], transports[name], protocol)
        git(root / RUNTIME, "config", "--local", f"{key}.active", "true")
        git(root / RUNTIME, "config", "--local", f"{key}.url", sources[name]["origin"])
    git(root / RUNTIME, "submodule", "absorbgitdirs")
    return verify(root, sources)


def prepare_downstream(root, sources, branch):
    verify(root, sources)
    links = dependencies(root / RUNTIME, sources)
    repositories = {PLATFORM: root / PLATFORM, RUNTIME: root / RUNTIME,
                    **{n: root / RUNTIME / path for n, (path, _) in links.items()}}
    for repo in repositories.values():
        if git(repo, "for-each-ref", "--format=%(refname)", f"refs/heads/{branch}"):
            raise ValueError(f"{repo}: local branch {branch} already exists")
    for repo in repositories.values():
        git(repo, "switch", "--create", branch)
    runtime = repositories[RUNTIME]
    keys = git(runtime, "config", "--file=.gitmodules", "--name-only", "--list").splitlines()
    for name, (_, key) in links.items():
        git(runtime, "config", "--file=.gitmodules", "--replace-all", f"{key}.url", sources[name]["origin"])
        if f"{key}.branch" in keys:
            git(runtime, "config", "--file=.gitmodules", "--unset-all", f"{key}.branch")
    return {"branch": branch, "runtime_diff": git(runtime, "diff", "--", ".gitmodules"),
            "gitlinks": {name: sources[name]["commit"] for name in links}}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--lock", type=Path, default=DEFAULT_LOCK)
    subcommands = parser.add_subparsers(dest="operation", required=True)
    for operation in ("checkout", "verify", "prepare-downstream"):
        subcommand = subcommands.add_parser(operation)
        subcommand.add_argument("--root", type=Path, required=True, help="parent of platform-smolvm and smolvm")
        if operation == "checkout":
            transport = subcommand.add_mutually_exclusive_group(required=True)
            transport.add_argument("--mirror-dir", type=Path)
            transport.add_argument("--https-source", choices=("upstream", "fork"))
    args = parser.parse_args()
    try:
        root = args.root.absolute()
        sources, branch = load_lock(args.lock)
        if args.operation == "checkout":
            result = checkout(root, sources, args.mirror_dir.resolve() if args.mirror_dir else None,
                              args.https_source)
        elif args.operation == "verify":
            result = verify(root, sources)
        else:
            result = prepare_downstream(root, sources, branch)
    except (OSError, ValueError, KeyError, TypeError) as error:
        parser.exit(1, f"{parser.prog}: {error}\n")
    print(json.dumps(result, indent=2))


if __name__ == "__main__":
    main()
