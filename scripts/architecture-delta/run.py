#!/usr/bin/env python3
"""Measure architecture on the exact immutable trees just checked by lint-delta."""
import argparse
import importlib.util
import json
from pathlib import Path
import sys

sys.dont_write_bytecode = True
HERE = Path(__file__).resolve().parent
STRICT = HERE.parent/'lint-delta'
sys.path.insert(0,str(STRICT))
spec = importlib.util.spec_from_file_location('strict_source_runner',STRICT/'run.py')
source = importlib.util.module_from_spec(spec)
spec.loader.exec_module(source)
from budget import compare, read_json


def execute(options):
    strict_file = options.lint_result.resolve()
    strict = read_json(strict_file)
    if type(strict) is not dict or strict.get('passed') is not True:
        raise ValueError('architecture measurement requires successful strict source preparation')
    if any(type(strict.get(k)) is not str or source.re.fullmatch('[0-9a-f]{40}',strict[k]) is None
           or strict[k] == '0'*40 for k in ['base','head']):
        raise ValueError('strict source lacks exact base/head commit identities')
    if type(strict.get('trees')) is not dict or any(type(strict['trees'].get(k)) is not dict
           or type(strict['trees'][k].get('source_fingerprint')) is not dict for k in ['base','head']):
        raise ValueError('strict source lacks complete tree fingerprints')
    tools = options.tools.resolve()
    output = options.output.resolve()
    output.mkdir(mode=0o700)
    options.owns_output = True
    go = source.shutil.which('go')
    if go is None:
        raise ValueError('Go tool is not available')
    paths = source.go_paths(str(Path(go).resolve()))
    if strict['platform'] != {k:paths[k] for k in ['GOOS','GOARCH','CGO_ENABLED','GOVERSION']}:
        raise ValueError('architecture and strict build identities differ')
    binary = tools/'arch-metrics'
    binary_hash = source.digest(binary)
    facts, commands = {}, {}
    for label in ['base','head']:
        tree = strict_file.parent/'trees'/label
        if source.git(tree,'rev-parse','HEAD') != strict[label]:
            raise ValueError('architecture tree commit differs from strict source')
        before = source.source_fingerprint(tree)
        if before != strict['trees'][label]['source_fingerprint']:
            raise ValueError('architecture input differs from checked strict source')
        env = source.environment(paths,output/(label+'-home'),output/(label+'-unused-cache'),tools/'golangci-lint',output/'unused.json')
        raw = output/(label+'.json')
        commands[label] = source.run_command([str(binary),'-C',str(tree),'-json'],tree,env,raw)
        if source.source_fingerprint(tree) != before or source.git(tree,'status','--porcelain','--untracked-files=all'):
            raise ValueError('architecture analysis changed its source')
        facts[label] = read_json(raw)
        if type(facts[label]) is not dict or facts[label].get('build') != strict['platform']:
            raise ValueError('snapshot build identity differs from checked strict source')
        if source.digest(binary) != binary_hash:
            raise ValueError('architecture binary changed during measurement')
    result = compare(strict['base'],facts['base'],facts['head'],read_json(options.approvals))
    proof = dict(result=result,passed=result['passed'],base=strict['base'],head=strict['head'],
                 strict_result_sha256=source.digest(strict_file),snapshot_binary_sha256=binary_hash,
                 approvals_sha256=source.digest(options.approvals),commands=commands,
                 scripts_sha256={n:source.digest(HERE/n) for n in ['run.py','budget.py']})
    (output/'result.json').write_text(json.dumps(proof,indent=2)+'\n')
    print(json.dumps(result,sort_keys=True))
    return 0 if result['passed'] else 1


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--lint-result',type=Path,required=True)
    parser.add_argument('--tools',type=Path,required=True)
    parser.add_argument('--output',type=Path,required=True)
    parser.add_argument('--approvals',type=Path,default=HERE/'approved.json')
    options = parser.parse_args()
    try:
        return execute(options)
    except (OSError,ValueError,RuntimeError,source.subprocess.SubprocessError) as error:
        if getattr(options,'owns_output',False):
            (options.output/'failure.json').write_text(json.dumps({'passed':False,'error':str(error)},indent=2)+'\n')
        print('architecture delta failed: '+str(error),file=sys.stderr)
        return 2


if __name__ == '__main__':
    sys.exit(main())
