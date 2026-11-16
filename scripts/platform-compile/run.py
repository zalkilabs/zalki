#!/usr/bin/env python3
"""Compile the two original Linux CGO-disabled profiles without running tests."""
import argparse
import importlib.util
import json
import os
from pathlib import Path
import re
import signal
import stat
import subprocess
import sys
import time

sys.dont_write_bytecode = True
HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE.parent/'lint-delta'))
spec = importlib.util.spec_from_file_location('compile_source', HERE.parent/'lint-delta/run.py')
source = importlib.util.module_from_spec(spec)
spec.loader.exec_module(source)


def require(condition, message):
    if not condition:
        raise ValueError(message)


def write(path, value):
    path.write_text(json.dumps(value, indent=2)+'\n')


def json_values(raw):
    def unique(items):
        result = {}
        for key, value in items:
            require(key not in result, 'duplicate discovery JSON key')
            result[key] = value
        return result
    decoder = json.JSONDecoder(object_pairs_hook=unique)
    rows, remaining = [], raw.strip()
    while remaining:
        value, end = decoder.raw_decode(remaining)
        rows.append(value)
        remaining = remaining[end:].strip()
    require(rows, 'empty discovery')
    return rows


def command(argv, repo, env, folder, name, timeout=300):
    """Separate streams and a durable exit receipt, including failed commands."""
    out, err = folder/(name+'.stdout'), folder/(name+'.stderr')
    receipt = dict(argv=argv, cwd=str(repo), profile={k:env[k] for k in ['GOOS','GOARCH','CGO_ENABLED']},
                   exit=None, timed_out=False)
    started = time.monotonic()
    try:
        with out.open('xb') as stdout, err.open('xb') as stderr:
            child = subprocess.Popen(argv, cwd=repo, env=env, stdout=stdout, stderr=stderr,
                                     start_new_session=True)
            try:
                receipt['exit'] = child.wait(timeout=timeout)
            except subprocess.TimeoutExpired:
                receipt['timed_out'] = True
                # The owned leader is still unreaped here; never signal a read PID.
                try:
                    os.killpg(child.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
                receipt['exit'] = child.wait(timeout=10)
    except (OSError, subprocess.SubprocessError) as error:
        receipt['error'] = str(error)
        raise
    finally:
        receipt['seconds'] = time.monotonic()-started
        for key, path in [('stdout', out), ('stderr', err)]:
            if path.exists():
                receipt[key+'_sha256'] = source.digest(path)
        write(folder/(name+'.receipt.json'), receipt)
    require(not receipt['timed_out'] and receipt['exit'] == 0,
            'compile command failed; see '+str(folder/(name+'.receipt.json')))
    return receipt, out.read_text(), err.read_text()


def source_directories(repo):
    """The same directory exclusions as Go's ./... package walk."""
    result, nested = set(), []
    for directory, dirs, files in os.walk(repo, followlinks=False):
        path = Path(directory)
        if path != repo and 'go.mod' in files:
            nested.append(path.relative_to(repo).as_posix())
            dirs[:] = []
            continue
        dirs[:] = sorted(n for n in dirs if n != 'vendor' and n != 'testdata' and not n.startswith(('.', '_')))
        if any(n.endswith('.go') and not n.startswith(('.', '_')) for n in files):
            result.add(path.relative_to(repo).as_posix())
    return result, nested


def classify(row, repo, module):
    require(type(row) is dict and type(row.get('Dir')) is str and type(row.get('ImportPath')) is str,
            'incomplete package discovery')
    directory = Path(row['Dir'])
    require(directory.is_absolute() and directory == directory.resolve(), 'linked/nonabsolute package directory')
    require(directory == repo or repo in directory.parents, 'package directory outside source')
    relative = directory.relative_to(repo).as_posix()
    expected = module+('/'+relative if relative != '.' else '')
    require(row['ImportPath'] == expected, 'package import identity differs from source')
    for key in ['GoFiles', 'CgoFiles', 'TestGoFiles', 'XTestGoFiles', 'IgnoredGoFiles']:
        files = row.get(key, [])
        require(type(files) is list and len(files) == len(set(files)) and
                all(type(n) is str and n.endswith('.go') and '/' not in n and '\\' not in n and
                    n not in ['.', '..'] and (directory/n).is_file() for n in files),
                'invalid package file inventory')
    require(not row.get('CgoFiles'), 'CGO-disabled discovery selected CgoFiles')
    inventory = [name for key in ['GoFiles','CgoFiles','TestGoFiles','XTestGoFiles','IgnoredGoFiles']
                 for name in row.get(key, [])]
    current = {p.name for p in directory.iterdir() if p.is_file() and p.name.endswith('.go') and
               not p.name.startswith(('.', '_'))}
    require(not row.get('InvalidGoFiles') and len(inventory) == len(set(inventory)) and set(inventory) == current,
            'package file inventory omitted or duplicated current source')
    require(not row.get('DepsErrors'), 'package discovery has dependency errors')
    error = row.get('Error')
    if error:
        require(type(error) is dict and error.get('Err') ==
                'build constraints exclude all Go files in '+str(directory) and row.get('IgnoredGoFiles') and
                not any(row.get(k) for k in ['GoFiles','TestGoFiles','XTestGoFiles']),
                'package discovery contains an error')
        return relative, None
    require(row.get('Incomplete', False) is False and
            any(row.get(k) for k in ['GoFiles','TestGoFiles','XTestGoFiles']),
            'incomplete or empty package inventory')
    return relative, row


def discover(go, repo, env, folder, module):
    _, stdout, stderr = command([go,'list','-mod=readonly','-json','./...'], repo, env, folder, 'discovery')
    require(not stderr.strip(), 'discovery wrote stderr')
    packages, excluded = {}, []
    for row in json_values(stdout):
        relative, selected = classify(row, repo, module)
        require(selected is not None, 'wildcard discovery contains an excluded/error package')
        require(relative not in packages, 'duplicate discovered package')
        packages[relative] = selected
    directories, nested = source_directories(repo)
    require(set(packages) <= directories, 'discovery reported a directory without source')
    # A well-formed partial successful wildcard list is also refused: every
    # omitted source directory needs an explicit target-exclusion witness.
    for index, name in enumerate(sorted(directories-set(packages))):
        _, raw, stderr = command([go,'list','-mod=readonly','-e','-json','./'+name],
                                  repo, env, folder, 'omitted-'+str(index))
        require(not stderr.strip(), 'omitted-directory discovery wrote stderr')
        rows = json_values(raw)
        require(len(rows) == 1, 'omitted-directory discovery is ambiguous')
        relative, selected = classify(rows[0], repo, module)
        require(relative == name and selected is None, 'wildcard discovery omitted a selected package')
        excluded.append(dict(directory=name, reason='target_build_constraints', witness=rows[0]))
    tests = [row for _, row in sorted(packages.items()) if row.get('TestGoFiles') or row.get('XTestGoFiles')]
    require(packages and tests, 'empty package/test-bearing selection')
    result = dict(packages=list(packages.values()), test_packages=[r['ImportPath'] for r in tests],
                  excluded=excluded, nested_modules=nested)
    write(folder/'package-inventory.json', result)
    return result


def execute(repo, output, go):
    repo, output = repo.resolve(), output.resolve()
    require(output != repo and repo not in output.parents, 'output must be outside source')
    require(not output.exists(), 'output already exists')
    head = source.git(repo, 'rev-parse', 'HEAD')
    tree = source.git(repo, 'rev-parse', 'HEAD^{tree}')
    require(re.fullmatch('[0-9a-f]{40}', head) and re.fullmatch('[0-9a-f]{40}', tree), 'missing exact source identity')
    require(not source.git(repo, 'status', '--porcelain', '--untracked-files=all'), 'source must be clean')
    before = source.source_fingerprint(repo)
    output.mkdir(mode=0o700)
    result = dict(passed=False, head=head, tree=tree, source_fingerprint=before, profiles={},
                  executable_artifacts='hashes recorded; test/CLI binaries never executed or uploaded')
    try:
        paths = source.go_paths(go)
        require(paths['GOOS'] == 'linux', 'compile task must run on the existing Ubuntu job')
        go = str(Path(paths['GOROOT'])/'bin/go')
        tool_hash = source.digest(Path(go))
        result['tool'] = dict(path=go, sha256=tool_hash, bootstrap=paths)
        module_lines = re.findall(r'^module\s+(\S+)\s*$', (repo/'go.mod').read_text(), re.M)
        require(len(module_lines) == 1, 'module identity is missing/ambiguous')
        module = module_lines[0]
        for arch in ['amd64', 'arm64']:
            folder = output/arch
            folder.mkdir(mode=0o700)
            env = source.environment(paths, folder/'home', folder/'unused-linter-cache',
                                     folder/'unused-linter', folder/'unused-report.json')
            env.update(GOOS='linux', GOARCH=arch, CGO_ENABLED='0', GOFLAGS='')
            temporary = folder/'tmp'
            temporary.mkdir(mode=0o700)
            env.update(TMPDIR=str(temporary), TMP=str(temporary), TEMP=str(temporary))
            def run(args, name):
                receipt, stdout, stderr = command([go, *args], repo, env, folder, name)
                require(source.source_fingerprint(repo) == before, 'compile command changed source')
                return receipt, stdout, stderr
            _, raw, stderr = run(['env','-json','GOOS','GOARCH','CGO_ENABLED','GOVERSION'], 'profile')
            require(not stderr.strip(), 'profile query wrote stderr')
            actual = json_values(raw)
            expected = dict(GOOS='linux', GOARCH=arch, CGO_ENABLED='0', GOVERSION=paths['GOVERSION'])
            require(actual == [expected], 'actual compiler profile differs')
            profile = dict(build=expected, test_binaries=[])
            result['profiles'][arch] = profile
            if arch == 'amd64':
                profile['inventory'] = discover(go, repo, env, folder, module)
                require(source.source_fingerprint(repo) == before, 'discovery changed source')
            run(['vet','-p=1','-mod=readonly', './...' if arch == 'amd64' else './cmd/zalki'], 'vet')
            build = folder/'build'
            build.mkdir(mode=0o700)
            run(['build','-p=1','-mod=readonly','-o',str(build)+os.sep,'./...'], 'build')
            outputs = sorted(build.iterdir())
            require((build/'zalki') in outputs and all(p.is_file() and not p.is_symlink() and
                    p.stat().st_size > 0 for p in outputs), 'CLI link output missing/invalid')
            profile['build_outputs'] = {p.name:source.digest(p) for p in outputs}
            if arch == 'amd64':
                for index, package in enumerate(profile['inventory']['test_packages']):
                    binary = folder/('test-'+str(index)+'.bin')
                    run(['test','-c','-p=1','-mod=readonly','-o',str(binary),package], 'test-'+str(index))
                    require(binary.is_file() and not binary.is_symlink() and binary.stat().st_size > 0,
                            'test compile output missing/invalid')
                    profile['test_binaries'].append(dict(package=package, sha256=source.digest(binary), bytes=binary.stat().st_size))
            require(source.digest(Path(go)) == tool_hash, 'compiler binary changed')
        require(source.source_fingerprint(repo) == before and not source.git(repo, 'status','--porcelain','--untracked-files=all'),
                'source changed during compile-only task')
        result['passed'] = True
    except (OSError, ValueError, TypeError, KeyError, subprocess.SubprocessError) as error:
        result['error'] = str(error)
        raise
    finally:
        try:
            result['source_after'] = source.source_fingerprint(repo)
            result['source_unchanged'] = result['source_after'] == before
            if not result['source_unchanged']:
                result['passed'] = False
                result.setdefault('error', 'source changed during compile-only task')
        except (OSError, ValueError) as error:
            result['passed'] = False
            result['source_after_error'] = str(error)
        write(output/'result.json', result)
    require(result['passed'], 'compile source closure failed; see '+str(output/'result.json'))
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--repo', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    try:
        go = source.shutil.which('go')
        require(go is not None, 'Go is unavailable')
        execute(args.repo, args.output, go)
        return 0
    except (OSError, ValueError, TypeError, KeyError, subprocess.SubprocessError) as error:
        print('CGO-disabled compile task failed: '+str(error), file=sys.stderr)
        return 2


if __name__ == '__main__':
    sys.exit(main())
