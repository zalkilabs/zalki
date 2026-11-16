#!/usr/bin/env python3
"""Capture one native human architecture report and quiet full-module coverage."""
import argparse
import importlib.util
import json
import re
from pathlib import Path
import subprocess
import sys

sys.dont_write_bytecode = True
HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE.parent/'fault-controls'))


def load(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


capture = load('checkpoint_capture', HERE.parent/'platform-compile/run.py')
unraced = load('checkpoint_unraced', HERE.parent/'fault-controls/unraced.py')
source, require = capture.source, capture.require
import cli


def cli_command(profile):
    """Adapt existing failure-durable capture to the existing CLI build helper."""
    def run(argv, cwd, env, log, expected, timeout=120):
        require(expected == 0, 'checkpoint commands must succeed')
        # The CLI helper intentionally omits GOOS/GOARCH; receipts use the exact
        # queried native SDK profile, without guessing or changing selection.
        env = dict(env, **profile)
        receipt, stdout, stderr = capture.command(argv, cwd, env, log.parent, log.name, timeout)
        log.write_text(stdout+stderr)
        return dict(receipt, log_sha256=source.digest(log))
    return run


def inventory(go, tree, env, output):
    def discover(pattern, label):
        _, raw, stderr = capture.command([go, 'list', '-mod=readonly', '-e', '-json', pattern],
                                          tree, env, output, label)
        require(not stderr.strip(), 'native discovery wrote stderr')
        return unraced.json_values(raw)
    rows = discover('./...', 'packages')
    directories, nested = capture.source_directories(tree)
    found, selected, excluded = set(), [], []

    def accept(row, omitted=False):
        relative, package, reason = unraced.classify(row, tree)
        require(relative in directories and relative not in found, 'duplicate/foreign package directory')
        require(not row.get('DepsErrors') and not row.get('InvalidGoFiles'), 'invalid package dependencies/files')
        actual = {p.name for p in (tree/relative).glob('*.go') if not p.name.startswith(('.', '_'))}
        files = []
        for key in ['GoFiles', 'CgoFiles', 'TestGoFiles', 'XTestGoFiles', 'IgnoredGoFiles']:
            values = row.get(key, [])
            require(type(values) is list and all(type(v) is str and v.endswith('.go') and
                    '/' not in v and '\\' not in v for v in values), 'invalid native file inventory')
            files.extend(values)
        require(len(files) == len(set(files)) and set(files) == actual, 'incomplete native file inventory')
        require(not omitted or package is None, 'wildcard discovery omitted a selected package')
        found.add(relative)
        if package is None:
            excluded.append(dict(directory=relative, reason=reason, witness=row))
        else:
            require(not row.get('Incomplete'), 'incomplete native package')
            selected.append(package)

    for row in rows:
        accept(row)
    for index, name in enumerate(sorted(directories-found)):
        probe = discover('./'+name, 'omitted-'+str(index))
        require(len(probe) == 1 and probe[0].get('Dir') == str(tree/name), 'ambiguous omitted package witness')
        accept(probe[0], omitted=True)
    require(found == directories and selected, 'empty/incomplete full module discovery')
    result = dict(packages=selected, exclusions=excluded, nested_modules=nested)
    capture.write(output/'package-inventory.json', result)
    return selected


def human_report(raw, paths, packages):
    require(raw.startswith('Build: %s/%s, %s, CGO_ENABLED=%s.' %
                           tuple(paths[k] for k in ['GOOS', 'GOARCH', 'GOVERSION', 'CGO_ENABLED'])),
            'human report native context differs')
    for section in ['## Packages', '| Package | Files | Source lines | Test lines |',
                    '## Functions over 60 lines', '## Internal imports']:
        require(section in raw, 'missing human report section: '+section)
    measured = {}
    for line in raw.splitlines():
        if not line.startswith(('| cmd/', '| internal/')):
            continue
        cells = [v.strip() for v in line.strip('|').split('|')]
        require(len(cells) == 6 and all(v.isdecimal() for v in cells[1:5]) and
                cells[0] not in measured, 'malformed/duplicate human package row')
        measured[cells[0]] = [int(v) for v in cells[1:4]]
    expected = {}
    def lines(path):
        raw = path.read_bytes()
        return raw.count(b'\n') + int(bool(raw) and not raw.endswith(b'\n'))
    for row in packages:
        name = row['ImportPath'].removeprefix(unraced.PREFIX+'/')
        if name.startswith(('cmd/', 'internal/')):
            folder = Path(row['Dir'])
            production = row.get('GoFiles', [])+row.get('CgoFiles', [])
            tests = row.get('TestGoFiles', [])+row.get('XTestGoFiles', [])
            expected[name] = [len(production), sum(lines(folder/f) for f in production),
                              sum(lines(folder/f) for f in tests)]
    require(expected and measured == expected, 'human report package/file/physical-line inventory differs')


def execute(repo, output, tools, go):
    repo, output, tools = repo.resolve(), output.resolve(), tools.resolve()
    require(repo not in output.parents and output != repo and not output.exists(), 'output must be fresh and outside source')
    head = source.git(repo, 'rev-parse', 'HEAD')
    require(re.fullmatch('[0-9a-f]{40}', head), 'missing exact source head')
    require(not source.git(repo, 'status', '--porcelain', '--untracked-files=all'), 'source must be clean')
    output.mkdir(mode=0o700)
    result = dict(passed=False, head=head, tree=source.git(repo, 'rev-parse', 'HEAD^{tree}'),
                  profile='separate native quiet coverage; no race/alias/unraced profile changed',
                  binaries='CLI hash/build metadata recorded; executables are not uploaded')
    tree = output/'source'
    try:
        cli.clone_source(repo, head, tree, source)
        before = source.source_fingerprint(tree)
        result['source_fingerprint'] = before
        paths = source.go_paths(go)
        require(paths['GOOS'] == 'darwin', 'checkpoint capture is the single native macOS profile')
        profile = {k: paths[k] for k in ['GOOS', 'GOARCH', 'CGO_ENABLED']}
        result['platform'] = {k: paths[k] for k in ['GOOS', 'GOARCH', 'CGO_ENABLED', 'GOVERSION']}
        go = str(Path(paths['GOROOT'])/'bin/go')
        result['tool_sha256'] = dict(go=source.digest(go), arch_metrics=source.digest(tools/'arch-metrics'))
        env = source.environment(paths, output/'home', output/'unused-cache', tools/'unused', output/'unused.json')
        env.update(profile)
        temporary = output/'tmp'
        temporary.mkdir(mode=0o700)
        env.update(TMPDIR=str(temporary), TMP=str(temporary), TEMP=str(temporary))
        _, raw, _ = capture.command([go, 'env', '-json', 'GOOS', 'GOARCH', 'CGO_ENABLED', 'GOVERSION'],
                                     tree, env, output, 'native-context')
        require(json.loads(raw) == result['platform'], 'actual native context differs')
        _, raw, _ = capture.command([go, 'version', '-m', '-json', str(tools/'arch-metrics')],
                                     tree, env, output, 'architecture-tool-buildinfo')
        info = json.loads(raw)
        require(info.get('Path') == unraced.PREFIX+'/scripts/arch-metrics' and
                info.get('GoVersion') == paths['GOVERSION'], 'AST tool source/SDK identity differs')
        settings = info.get('Settings', [])
        require(type(settings) is list and all(type(v) is dict and type(v.get('Key')) is str and
                set(v) <= {'Key', 'Value'} and bool(v['Key']) and
                type(v.get('Value', '')) is str for v in settings), 'invalid AST tool build settings')
        values = {v['Key']:v.get('Value', '') for v in settings}
        require(len(values) == len(settings) and values.get('vcs.revision') == head and
                values.get('vcs.modified') == 'false' and all(values.get(k) == paths[k]
                for k in ['GOOS', 'GOARCH', 'CGO_ENABLED']), 'AST tool current native source identity differs')
        result['architecture_tool_buildinfo'] = info
        env.update(GOPROXY='https://proxy.golang.org', GOSUMDB='sum.golang.org')
        capture.command([go, 'mod', 'download'], tree, env, output, 'modules', 300)
        env.update(GOPROXY='off', GOSUMDB='off')
        packages = inventory(go, tree, env, output)
        _, raw, _ = capture.command([str(tools/'arch-metrics'), '-C', str(tree)],
                                     tree, env, output, 'human-architecture')
        human_report(raw, paths, packages)
        (output/'architecture.md').write_text(raw)
        binary, result['cli'] = cli.build(tree, head, paths, output/'cli', source, cli_command(profile))
        env['ZALKI_TEST_BINARY'] = str(binary)
        capture.command([go, 'run', '-mod=readonly', './scripts/check-integration', '--list-required'],
                                       tree, env, output, 'required-ledger')
        required = source.read_json(output/'required-ledger.stdout')
        _, raw, _ = capture.command([go, 'test', '-cover', '-count=1', '-p=1', '-json', '-timeout=10m',
                                     '-coverprofile='+str(output/'coverage.out'), './...'],
                                    tree, env, output, 'coverage', 900)
        result['test_events'] = unraced.coverage(raw, packages, required)
        require((output/'coverage.out').is_file() and (output/'coverage.out').stat().st_size > 0,
                'missing coverage profile')
        require(source.digest(binary) == result['cli']['binary_sha256'], 'fresh CLI changed')
        require(source.digest(go) == result['tool_sha256']['go'] and
                source.digest(tools/'arch-metrics') == result['tool_sha256']['arch_metrics'], 'analysis SDK/tool changed')
        result['coverage_sha256'] = source.digest(output/'coverage.out')
        result['passed'] = True
        return 0
    except (OSError, ValueError, RuntimeError, subprocess.SubprocessError) as error:
        result['error'] = str(error)
        raise
    finally:
        if tree.exists():
            result['source_after'] = source.source_fingerprint(tree)
            result['clean_after'] = not source.git(tree, 'status', '--porcelain', '--untracked-files=all')
            if result['source_after'] != result.get('source_fingerprint') or not result['clean_after']:
                result['passed'] = False
                result['source_changed'] = True
        capture.write(output/'result.json', result)
        require(not result.get('source_changed'), 'checkpoint capture changed its source')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ['repo', 'output', 'tools']:
        parser.add_argument('--'+name, type=Path, required=True)
    parser.add_argument('--go', default='go')
    options = parser.parse_args()
    try:
        return execute(options.repo, options.output, options.tools, options.go)
    except (OSError, ValueError, RuntimeError, subprocess.SubprocessError) as error:
        print('checkpoint capture failed: '+str(error), file=sys.stderr)
        return 2


if __name__ == '__main__':
    sys.exit(main())
