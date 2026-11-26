#!/usr/bin/env python3
"""Compare actual base/head lint in both make target orders with fresh caches."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shlex
import shutil
import signal
import stat
import subprocess
import sys
import time

sys.dont_write_bytecode = True

from compare import compare, read_json, read_report

HERE = Path(__file__).resolve().parent
GO_PATHS = ['GOCACHE', 'GOMODCACHE', 'GOPATH', 'GOROOT']
PLATFORM = ['GOOS', 'GOARCH', 'CGO_ENABLED']


def digest(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def source_fingerprint(root):
    rows = []
    for directory, directories, files in os.walk(root, followlinks=False):
        directories[:] = sorted(n for n in directories if n != '.git')
        for name in sorted(files + [n for n in directories if (Path(directory)/n).is_symlink()]):
            path = Path(directory)/name
            if name == '.git':
                continue
            relative = path.relative_to(root).as_posix()
            if path.is_symlink():
                raise ValueError('source symlinks are not supported: '+relative)
            else:
                rows.append([relative, stat.S_IMODE(path.stat().st_mode), digest(path)])
    payload = json.dumps(sorted(rows), separators=(',', ':')).encode()
    return dict(files=len(rows), sha256=hashlib.sha256(payload).hexdigest())


def run_command(arguments, directory, environment, log, timeout=900):
    started = time.monotonic()
    with log.open('xb') as output:
        child = subprocess.Popen(arguments, cwd=directory, env=environment,
                                 stdout=output, stderr=subprocess.STDOUT, start_new_session=True)
        try:
            status = child.wait(timeout=timeout)
        except subprocess.TimeoutExpired:
            # wait timed out without reaping this session leader: its PID still
            # owns the group identity. Never signal a previously reaped PID.
            try:
                os.killpg(child.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            child.wait(timeout=10)
            raise
    if status:
        raise RuntimeError('command failed (%d); see %s' % (status, log))
    return dict(argv=arguments, cwd=str(directory), seconds=time.monotonic()-started,
                log_sha256=digest(log), exit=status)


def git(repo, *arguments):
    return subprocess.check_output(['git', '-C', str(repo), *arguments], text=True).strip()


def revisions(repo, base, head, event):
    head = head or git(repo, 'rev-parse', 'HEAD')
    if not base or base == '0'*40:
        if event in ['pull_request', 'push']:
            raise ValueError('event has no usable base commit; refusing an invented baseline')
        base = git(repo, 'rev-parse', head + '^')
    for value in [base, head]:
        if re.fullmatch('[0-9a-f]{40}', value) is None or value == '0'*40:
            raise ValueError('base/head must be full commit IDs')
        if git(repo, 'rev-parse', '--verify', value + '^{commit}') != value:
            raise ValueError('base/head object does not match requested commit')
    return base, head


def go_paths(go):
    # Query only Go controls/cache locations. Arbitrary ambient GOFLAGS and
    # native-client variables never enter the tool or analyzed trees.
    env = {k: os.environ[k] for k in ['PATH', 'HOME', *GO_PATHS] if k in os.environ}
    env.update(GOENV='off', GOWORK='off', GOTOOLCHAIN='local')
    # The caller may choose the platform (for example GOOS=linux on a Mac).
    # Go must report exactly that, or the run is refused: no silent host default.
    requested = {k: os.environ[k] for k in PLATFORM if k in os.environ}
    env.update(requested)
    if os.environ.get('RUNNER_TRACKING_ID'):
        env['RUNNER_TRACKING_ID'] = os.environ['RUNNER_TRACKING_ID']
    names = [*GO_PATHS, 'CGO_ENABLED', 'GOOS', 'GOARCH', 'GOVERSION']
    paths = json.loads(subprocess.check_output([go, 'env', '-json', *names], env=env, text=True, timeout=30))
    for key in GO_PATHS:
        if type(paths.get(key)) is not str:
            raise ValueError('Go did not report ' + key)
        parts = paths[key].split(os.pathsep) if key == 'GOPATH' else [paths[key]]
        if any(not Path(part).is_absolute() for part in parts):
            raise ValueError('Go did not report an absolute ' + key)
        paths[key] = os.pathsep.join(str(Path(part).resolve()) for part in parts)
    if paths.get('CGO_ENABLED') not in ['0', '1'] or any(not paths.get(k) for k in ['GOOS', 'GOARCH', 'GOVERSION']):
        raise ValueError('Go platform identity is incomplete')
    for key, value in requested.items():
        if paths[key] != value:
            raise ValueError('requested %s=%r but Go reports %r' % (key, value, paths[key]))
    return paths


def environment(paths, home, cache, linter, report):
    home.mkdir(mode=0o700)
    values = {k: paths[k] for k in GO_PATHS}
    values.update(PATH=str(Path(paths['GOROOT'])/'bin') + ':/usr/bin:/bin:/usr/sbin:/sbin',
                  HOME=str(home), CODEX_HOME=str(home/'codex'), CLAUDE_CONFIG_DIR=str(home/'claude'),
                  CURSOR_CONFIG_DIR=str(home/'cursor'), XDG_CONFIG_HOME=str(home/'config'),
                  XDG_CACHE_HOME=str(home/'cache'), LANG='C', LC_ALL='C',
                  GOOS=paths['GOOS'], GOARCH=paths['GOARCH'],
                  GOENV='off', GOWORK='off', GOTOOLCHAIN='local', GOTELEMETRY='off',
                  GOFLAGS='-mod=readonly -p=2', GOMAXPROCS='2', CGO_ENABLED=paths['CGO_ENABLED'],
                  GOPROXY='off', GOSUMDB='off', GOPRIVATE='', GONOPROXY='none',
                  GONOSUMDB='none', GOVCS='*:off', GIT_CONFIG_GLOBAL='/dev/null',
                  GIT_CONFIG_SYSTEM='/dev/null', ZALKI_LIVE_SMOKE='0',
                  GOLANGCI_LINT_CACHE=str(cache), LINT_REAL_BINARY=str(linter), STRICT_REPORT=str(report))
    # Retain the isolated CI runner's descendant-cleanup tracking, never log it.
    if os.environ.get('RUNNER_TRACKING_ID'):
        values['RUNNER_TRACKING_ID'] = os.environ['RUNNER_TRACKING_ID']
    for key in ['CODEX_HOME', 'CLAUDE_CONFIG_DIR', 'CURSOR_CONFIG_DIR', 'XDG_CONFIG_HOME', 'XDG_CACHE_HOME']:
        Path(values[key]).mkdir(mode=0o700)
    return values


def prepare_tree(repo, commit, tree, output, paths, tools):
    tree.parent.mkdir(exist_ok=True)
    subprocess.run(['git', '-C', str(repo), 'worktree', 'add', '--detach', str(tree), commit], check=True)
    if git(tree, 'rev-parse', 'HEAD') != commit:
        raise ValueError('worktree commit differs from requested source')
    before = source_fingerprint(tree)
    env = environment(paths, output/'download-home', output/'unused-cache', tools/'golangci-lint', output/'unused.json')
    # Warm both actual module sets before offline lint; never assume HEAD's
    # dependencies include every version still needed by the base tree.
    env.update(GOPROXY='https://proxy.golang.org', GOSUMDB='sum.golang.org')
    row = run_command([str(Path(paths['GOROOT'])/'bin/go'), 'mod', 'download'], tree, env, output/'modules.log')
    if source_fingerprint(tree) != before:
        raise ValueError('module preparation changed its source tree')
    return dict(commit=commit, tree=git(tree, 'rev-parse', 'HEAD^{tree}'), source_fingerprint=before,
                makefile_sha256=digest(tree/'Makefile'), config_sha256=digest(tree/'.golangci.yml'), modules=row)


def lint_order(tree, directory, targets, paths, tools):
    directory.mkdir()
    before = source_fingerprint(tree)
    cache = directory/'linter-cache'
    cache.mkdir()
    if any(cache.iterdir()):
        raise ValueError('linter cache is not initially fresh')
    raw, scoped = directory/'strict.json', directory/'scoped.json'
    env = environment(paths, directory/'home', cache, tools/'golangci-lint', raw)
    wrapper = shlex.join([sys.executable, str(HERE/'capture.py')])
    command = ['make', *targets, 'GOLANGCI_LINT='+wrapper, 'GOCOGNIT='+shlex.quote(str(tools/'gocognit'))]
    row = run_command(command, tree, env, directory/'make.log')
    # The scoper validates actual Go syntax and source positions; no guessed
    # declaration regex or historical line-number normalization is involved.
    envelope = read_json(raw)
    if not isinstance(envelope, dict) or 'Issues' not in envelope or (envelope['Issues'] is not None and not isinstance(envelope['Issues'], list)):
        raise ValueError('strict JSON must contain an explicit issue collection')
    scope = run_command([str(tools/'issue-scopes'), str(tree), str(raw)], tree, env, scoped)
    if source_fingerprint(tree) != before or git(tree, 'status', '--porcelain', '--untracked-files=all'):
        raise ValueError('analysis changed its source tree')
    return read_report(scoped), dict(make=row, scope=scope, strict_sha256=digest(raw),
                                    scoped_sha256=digest(scoped), fresh_cache=str(cache))


def execute(options):
    repo, output, tools = options.repo.resolve(), options.output.resolve(), options.tools.resolve()
    base, head = revisions(repo, options.base, options.head, options.event)
    output.mkdir(mode=0o700)  # Refuse an existing report/cache set or stale success.
    options.owns_output = True
    go = shutil.which('go')
    if go is None:
        raise ValueError('Go tool is not available')
    paths = go_paths(str(Path(go).resolve()))
    proof = dict(base=base, head=head, platform={k:paths[k] for k in ['GOOS','GOARCH','CGO_ENABLED','GOVERSION']},
                 tool_sha256={n:digest(tools/n) for n in ['golangci-lint','gocognit','issue-scopes']},
                 script_sha256={n:digest(HERE/n) for n in ['run.py','capture.py','compare.py']}, trees={}, orders={})
    (output/'manifest.json').write_text(json.dumps(proof,indent=2)+'\n')
    reports = []
    for label, commit in [('base',base),('head',head)]:
        directory = output/label; directory.mkdir()
        tree = output/'trees'/label
        proof['trees'][label] = prepare_tree(repo,commit,tree,directory,paths,tools)
        for order, targets in [('normal-first',['lint','lint-strict']),('strict-first',['lint-strict','lint'])]:
            report, row = lint_order(tree,directory/order,targets,paths,tools)
            reports.append(report); proof['orders'][label+'-'+order] = row
    result = compare(*reports)
    proof.update(result=result, passed=result['passed'], initially_fresh_linter_caches=4)
    (output/'result.json').write_text(json.dumps(proof,indent=2)+'\n')
    print(json.dumps(result,sort_keys=True))
    return 0 if result['passed'] else 1


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--repo',type=Path,default=Path.cwd())
    parser.add_argument('--base',default='')
    parser.add_argument('--head',default='')
    parser.add_argument('--event',required=True,choices=['pull_request','push','workflow_dispatch','workflow_call','tag'])
    parser.add_argument('--tools',type=Path,required=True)
    parser.add_argument('--output',type=Path,required=True)
    options = parser.parse_args()
    try:
        return execute(options)
    except (OSError,ValueError,RuntimeError,subprocess.SubprocessError) as error:
        if getattr(options, 'owns_output', False):
            (options.output/'failure.json').write_text(json.dumps({'passed':False,'error':str(error)},indent=2)+'\n')
        print('lint delta failed: '+str(error),file=sys.stderr)
        return 2


if __name__ == '__main__':
    sys.exit(main())
