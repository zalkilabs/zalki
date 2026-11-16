#!/usr/bin/env python3
"""Full unraced tests of affected native packages with a fresh source-bound CLI."""
import argparse
import importlib.util
import json
from pathlib import Path,PurePosixPath
import subprocess
import sys

sys.dont_write_bytecode=True
HERE=Path(__file__).resolve().parent
spec=importlib.util.spec_from_file_location('focused_control_runner',HERE/'run.py')
controls=importlib.util.module_from_spec(spec);spec.loader.exec_module(controls)
source,require=controls.source,controls.require
from oracle import descendant
from cli import identity as cli_identity,clone_source,build as build_cli
PREFIX='github.com/zalkilabs/zalki'


def json_values(raw):
    def unique(items):
        result={}
        for key,value in items:
            require(key not in result,'duplicate discovery/report JSON key');result[key]=value
        return result
    decoder=json.JSONDecoder(object_pairs_hook=unique)
    rows=[];remaining=raw.strip()
    while remaining:
        value,end=decoder.raw_decode(remaining);rows.append(value);remaining=remaining[end:].strip()
    require(rows,'empty native package discovery')
    return rows


def classify(row,tree):
    require(type(row) is dict and type(row.get('Dir')) is str and type(row.get('ImportPath')) is str,'incomplete package discovery')
    directory=Path(row['Dir']);require(directory.is_absolute(),'package directory must be absolute')
    relative=directory.relative_to(tree).as_posix()
    expected=PREFIX+('/'+relative if relative!='.' else '')
    require(row['ImportPath']==expected,'package import path is outside actual source')
    for key in ['TestGoFiles','XTestGoFiles','IgnoredGoFiles']:
        files=row.get(key,[])
        require(type(files) is list and all(type(name) is str and name and '/' not in name and '\\' not in name for name in files),
                'invalid native package file inventory')
    tests=row.get('TestGoFiles',[])+row.get('XTestGoFiles',[])
    error=row.get('Error')
    if error:
        # Go's versioned cmd/go/internal/load.NoGoError classes. Unknown errors
        # always refuse; a successful partial discovery is not coverage.
        text=error.get('Err') if type(error) is dict else None
        if text=='build constraints exclude all Go files in '+str(directory) and row.get('IgnoredGoFiles'):
            return relative,None,'native_build_constraints'
        if text=='no non-test Go files in '+str(directory) and tests:
            return relative,row,None
        raise ValueError('native discovery failed: '+str(text))
    require(not row.get('DepsErrors') and not row.get('Incomplete'),'native discovery contains incomplete dependencies')
    return relative,row,None


def input_policy(value):
    if value is None:return dict(paths={},prefixes={},non_runtime_paths=[],non_runtime_prefixes=[])
    require(type(value) is dict and set(value)=={'paths','prefixes','non_runtime_paths','non_runtime_prefixes'},'invalid input ownership policy')
    for kind in ['paths','prefixes']:
        require(type(value[kind]) is dict,'invalid input ownership mapping')
        for name,owners in value[kind].items():
            require(controls.local_path(name[:-1] if kind=='prefixes' else name) and
                    (kind!='prefixes' or name.endswith('/')),'input mapping path escapes source')
            require(type(owners) is list and owners and len(set(owners))==len(owners) and
                    all(controls.local_path(owner) for owner in owners),'invalid owning package list')
    for kind in ['non_runtime_paths','non_runtime_prefixes']:
        require(type(value[kind]) is list and all(controls.local_path(name[:-1] if kind.endswith('prefixes') else name) and
                    (not kind.endswith('prefixes') or name.endswith('/')) for name in value[kind]),'invalid non-runtime input classification')
    return value


def nearest_package(tree,name,boundaries):
    parts=PurePosixPath(name).parts
    # Go testdata programs are inputs to their containing package, not packages
    # to compile independently, even when a fixture contains its own Go files.
    end=parts.index('testdata') if 'testdata' in parts else len(parts)-1
    for index in range(end,0,-1):
        candidate=PurePosixPath(*parts[:index]).as_posix()
        if candidate in boundaries or list((tree/candidate).glob('*.go')):return candidate
    return '.' if '.' in boundaries else None


def input_directories(tree,changed,packages,excluded,plan,policy,bindings):
    policy=input_policy(policy)
    boundaries=set(packages)|set(excluded)|{str(PurePosixPath(name).parent) for name in changed if name.endswith('.go') and 'testdata' not in PurePosixPath(name).parts}
    result=set()
    for name in changed:
        require(controls.local_path(name),'changed input path escapes source')
        owners=set(policy['paths'].get(name,[]))
        for prefix,targets in policy['prefixes'].items():
            if name.startswith(prefix):owners.update(targets)
        owners.update(c['selection']['directory'].removeprefix('./') for c in plan if c['mutation']['path']==name)
        explicit=set(owners)
        nearest=nearest_package(tree,name,boundaries)
        if nearest is not None:owners.add(nearest)
        if name in ['go.mod','go.sum']:owners.update(packages)
        require(all(owner in boundaries or list((tree/owner).glob('*.go')) for owner in explicit),'mapped runtime input owner is missing from current source')
        if not owners:
            require(name in policy['non_runtime_paths'] or any(name.startswith(prefix) for prefix in policy['non_runtime_prefixes']),
                    'changed runtime input has no reviewed/current owning package: '+name)
        result.update(owners)
        if bindings is not None:bindings.append(dict(input=name,owners=sorted(owners),classification='package_input' if owners else 'reviewed_non_runtime'))
    return result


def affected(tree,changed,catalog,plan,probe,policy=None,bindings=None):
    packages={};excluded={}
    for row in catalog:
        directory,package,reason=classify(row,tree)
        require(directory not in packages and directory not in excluded,'duplicate discovered package directory')
        if package is None:excluded[directory]=reason
        else:packages[directory]=package
    dirs=input_directories(tree,changed,packages,excluded,plan,policy,bindings)
    selected=[];omitted=[]
    for directory in sorted(dirs):
        require(controls.local_path(directory),'changed package directory escapes source')
        if directory not in packages and directory not in excluded:
            path=tree/directory
            if not path.exists() or not list(path.glob('*.go')):
                omitted.append(dict(directory=directory,reason='no_head_go_files'));continue
            rows=probe(directory);require(len(rows)==1,'explicit package probe is not singular')
            actual,row,reason=classify(rows[0],tree);require(actual==directory,'package probe returned a different directory')
            if row is None:excluded[directory]=reason
            else:packages[directory]=row
        if directory in excluded:omitted.append(dict(directory=directory,reason=excluded[directory]))
        else:selected.append(packages[directory])
    return selected,omitted


def coverage(raw,packages,required):
    require(type(required) is list and required,'missing current required root ledger')
    require(all(type(r) is dict and set(r)=={'package','test'} and
                type(r['package']) is str and r['package'].startswith(PREFIX+'/') and
                type(r['test']) is str and r['test'].startswith('Test') for r in required),'invalid required root ledger')
    names={p['ImportPath']:p for p in packages};terminals={};ran=set();passed=set();skips=[];no_tests=set()
    roots={(r['package'],r['test']) for r in required if r['package'] in names}
    require(len({(r['package'],r['test']) for r in required})==len(required),'required ledger duplicates a root')
    for event in json_values(raw):
        require(type(event) is dict,'invalid unraced event')
        package,name,action=event.get('Package'),event.get('Test'),event.get('Action')
        require(type(action) is str and (name is None or type(name) is str),'invalid unraced action/test')
        require(package in names,'unraced report contains an unselected package')
        require(action not in ['fail','build-fail','build-output'],'unraced package/test failed')
        if name is None:
            if action in ['pass','skip']:terminals.setdefault(package,[]).append(action)
            if action=='output' and '[no test files]' in event.get('Output','') and package in event.get('Output',''):no_tests.add(package)
            continue
        if action=='run':ran.add((package,name))
        if action=='pass':passed.add((package,name))
        if action=='skip':
            require(not any(package==pkg and descendant(name,root) for pkg,root in roots),
                    'required unraced root/descendant skipped')
            skips.append(dict(package=package,test=name))
    for package,row in names.items():
        expected=['pass']
        if not row.get('TestGoFiles',[]) and not row.get('XTestGoFiles',[]) and terminals.get(package)==['skip']:
            require(package in no_tests,'package skip is not an attested no-test-files result');expected=['skip']
        require(terminals.get(package)==expected,'unraced package is missing its terminal result')
    require(roots<=ran and roots<=passed,'required unraced root is missing RUN/PASS')
    return dict(packages=len(names),required_roots=len(roots),nonrequired_skips=skips)



def execute(options):
    repo,output,tools=options.repo.resolve(),options.output.resolve(),options.tools.resolve()
    base,head=source.revisions(repo,options.base,options.head,options.event)
    output.mkdir(mode=0o700);options.owns_output=True
    tree=output/'source';clone_source(repo,head,tree,source)
    before=source.source_fingerprint(tree)
    go=source.shutil.which('go');require(go is not None,'Go is unavailable')
    paths=source.go_paths(str(Path(go).resolve()));require(paths['GOOS'] in ['darwin','linux'],'unraced job must run natively')
    go=str(Path(paths['GOROOT'])/'bin/go')
    env=source.environment(paths,output/'home',output/'unused-cache',tools/'golangci-lint',output/'unused.json')
    temporary=output/'tmp';temporary.mkdir(mode=0o700);env.update(TMPDIR=str(temporary),TMP=str(temporary),TEMP=str(temporary))
    env.update(GOPROXY='https://proxy.golang.org',GOSUMDB='sum.golang.org')
    modules=controls.command([go,'mod','download'],tree,env,output/'modules.log',0,timeout=300)
    env.update(GOPROXY='off',GOSUMDB='off')
    commands={}
    def discover(pattern,label):
        raw=output/(label+'.json')
        commands[label]=controls.command([go,'list','-e','-json',pattern],tree,env,raw,0)
        return json_values(raw.read_text())
    changed=source.git(repo,'diff','--no-renames','--name-only',base,head).splitlines()
    bindings=[];policy_file=HERE/'input-owners.json'
    rows,omitted=affected(tree,changed,discover('./...','native-packages'),source.read_json(HERE/'controls.json'),
        lambda directory:discover('./'+directory,'probe-'+str(len(commands))),source.read_json(policy_file),bindings)
    result=dict(base=base,head=head,source_fingerprint=before,platform={k:paths[k] for k in ['GOOS','GOARCH','CGO_ENABLED','GOVERSION']},
                modules=modules,input_bindings=bindings,input_policy_sha256=source.digest(policy_file),selected_packages=[p['ImportPath'] for p in rows],omitted_directories=omitted,commands=commands)
    if rows:
        binary,cli_record=build_cli(tree,head,paths,output/'cli',source,controls.command)
        commands['build-cli']=cli_record['build'];commands['cli-buildinfo']=cli_record['metadata']
        source.shutil.copyfile(output/'cli/build.log',output/'cli-build.log')
        source.shutil.copyfile(output/'cli/buildinfo.json',output/'cli-buildinfo.json')
        result['cli']=cli_record;result['cli_settings']=cli_record['settings']
        result['cli_sha256']=cli_record['binary_sha256'];env['ZALKI_TEST_BINARY']=str(binary)
        commands['required-ledger']=controls.command([str(tools/'required-coverage'),'--list-required'],tree,env,output/'required-ledger.json',0)
        raw=output/'unraced-test2json.log'
        commands['tests']=controls.command([go,'test','-race=false','-count=1','-p=1','-json','-timeout=10m',
                                            *[p['ImportPath'] for p in rows]],tree,env,raw,0,timeout=900)
        result['coverage']=coverage(raw.read_text(),rows,source.read_json(output/'required-ledger.json'))
        require(source.digest(binary)==result['cli_sha256'],'fresh CLI changed during package tests')
    require(source.source_fingerprint(tree)==before and not source.git(tree,'status','--porcelain','--untracked-files=all'),
            'unraced validation changed source')
    result.update(passed=True,executed_packages=len(rows),required_tool_sha256=source.digest(tools/'required-coverage'),
                  runner_sha256=source.digest(HERE/'unraced.py'),cli_helper_sha256=source.digest(HERE/'cli.py'))
    (output/'result.json').write_text(json.dumps(result,indent=2)+'\n');print(json.dumps(dict(passed=True,executed_packages=len(rows))))
    return 0


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    for name in ['repo','tools','output']:parser.add_argument('--'+name,type=Path,required=True)
    for name in ['base','head','event']:parser.add_argument('--'+name,required=True)
    options=parser.parse_args()
    try:return execute(options)
    except (OSError,ValueError,KeyError,TypeError,subprocess.SubprocessError) as error:
        if getattr(options,'owns_output',False):(options.output/'failure.json').write_text(json.dumps(dict(passed=False,error=str(error)))+'\n')
        print('unraced affected validation failed: '+str(error),file=sys.stderr);return 2


if __name__=='__main__':sys.exit(main())
