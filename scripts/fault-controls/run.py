#!/usr/bin/env python3
"""Run the reviewed focused controls on disposable, source-bound CI worktrees."""
import argparse
import importlib.util
import json
import os
from pathlib import Path
import re
import signal
import subprocess
import sys
import time
from pathlib import PurePosixPath

sys.dont_write_bytecode=True
HERE=Path(__file__).resolve().parent
sys.path.insert(0,str(HERE.parent/'lint-delta'))
spec=importlib.util.spec_from_file_location('strict_source_runner',HERE.parent/'lint-delta/run.py')
source=importlib.util.module_from_spec(spec);spec.loader.exec_module(source)
from oracle import require,sha,source_digest,validate


def local_path(name):
    return type(name) is str and name and not PurePosixPath(name).is_absolute() and '\\' not in name and \
        '..' not in PurePosixPath(name).parts and name==PurePosixPath(name).as_posix()


def validate_controls(controls):
    require(type(controls) is list and controls and len({c['id'] for c in controls})==len(controls),'missing/duplicate controls')
    prefix='github.com/zalkilabs/zalki/'
    for c in controls:
        require(c['kind'] in ['production','runtime_config'] and re.fullmatch('[A-Z0-9-]+',c['id']), 'invalid control kind/ID')
        selection,mutation,assertion=c['selection'],c['mutation'],c['assertion']
        require(selection['directory'].startswith(('./internal/','./cmd/')) and
                local_path(selection['directory'][2:]) and selection['package']==prefix+selection['directory'][2:],
                'control package/directory differs')
        require(re.fullmatch(r'Test[A-Za-z0-9_]+(?:/[^\x00-\x1f/]+)*',selection['test']), 'invalid exact test selection')
        require(local_path(mutation['path']) and type(mutation['old']) is str and mutation['old'] and
                type(mutation['new']) is str and type(mutation['required_occurrences']) is int and mutation['required_occurrences']==1,
                'invalid exact mutation')
        if c['kind']=='runtime_config':
            require(mutation['path']=='.goreleaser.yaml' and mutation['symbol']=='builds[0].ldflags','unreviewed runtime configuration target')
        else:
            require(mutation['path'].startswith(('internal/','cmd/')) and mutation['path'].endswith('.go') and
                    not mutation['path'].endswith('_test.go') and re.fullmatch('[A-Za-z_][A-Za-z0-9_]*',mutation['symbol']),
                    'mutation must target a production function')
        require(local_path(assertion['file']) and PurePosixPath(assertion['file']).parent.as_posix()==selection['directory'][2:] and
                assertion['file'].endswith('_test.go') and type(c['output_fragment']) is str and c['output_fragment'],
                'assertion source/fragment differs from selected package')


def command(argv,cwd,env,log,expected,timeout=120):
    started=time.monotonic()
    with log.open('xb') as output:
        child=subprocess.Popen(argv,cwd=cwd,env=env,stdout=output,stderr=subprocess.STDOUT,start_new_session=True)
        try:status=child.wait(timeout=timeout)
        except subprocess.TimeoutExpired:
            try:os.killpg(child.pid,signal.SIGKILL)
            except ProcessLookupError:pass
            child.wait(timeout=10)
            raise
    require(status==expected,'unexpected command outcome (%s); see %s' % (status,log))
    return dict(argv=argv,exit=status,seconds=time.monotonic()-started,log_sha256=source.digest(log))


def actual_files(tree,control):
    names={control['mutation']['path'],control['assertion']['file']}
    return {name:(tree/name).read_bytes().decode('utf-8') for name in sorted(names)}


def phase(tree,folder,control,paths,tools,files,intended):
    folder.mkdir()
    env=source.environment(paths,folder/'home',folder/'unused-cache',tools/'golangci-lint',folder/'unused.json')
    temporary=folder/'tmp';temporary.mkdir(mode=0o700);env.update(TMPDIR=str(temporary),TMP=str(temporary),TEMP=str(temporary))
    go=str(Path(paths['GOROOT'])/'bin/go')
    before=source.source_fingerprint(tree)
    binary=folder/'control.test'
    build=command([go,'test','-race','-p=1','-c','-o',str(binary),control['selection']['directory']],tree,env,folder/'build.log',0,timeout=300)
    require(source.source_fingerprint(tree)==before,'test compilation changed source')
    pattern='/'.join('^'+re.escape(part)+'$' for part in control['selection']['test'].split('/'))
    raw=folder/'test2json.log'
    argv=[go,'tool','test2json','-t','-p',control['selection']['package'],str(binary),'-test.v=test2json',
          '-test.timeout=45s','-test.run='+pattern]
    package=tree/control['selection']['directory'].removeprefix('./')
    execution=command(argv,package,env,raw,1 if intended else 0)
    require(source.source_fingerprint(tree)==before,'focused execution changed source')
    require(actual_files(tree,control)==files,'phase source differs from recorded compile input')
    return dict(build=dict(exit_code=0,compiled=True,binary_sha256=source.digest(binary),setup_ok=True,receipt=build),
        process_outcome=dict(exit_code=execution['exit'],timed_out=False,signalled=False,streams_complete=True,setup_ok=True,receipt=execution),
        source_files=files,source_digest=source_digest(files),test2json_ndjson=raw.read_text())


def run_control(repo,head,control,output,paths,tools):
    identity=control['id'];require(re.fullmatch('[A-Z0-9-]+',identity) is not None,'invalid control ID')
    folder=output/identity;folder.mkdir();tree=folder/'source'
    subprocess.run(['git','-C',str(repo),'worktree','add','--detach',str(tree),head],check=True)
    require(source.git(tree,'rev-parse','HEAD')==head,'fault source HEAD differs')
    initial=source.source_fingerprint(tree)
    env=source.environment(paths,folder/'home',folder/'unused-lint-cache',tools/'golangci-lint',folder/'unused.json')
    temporary=folder/'tmp';temporary.mkdir(mode=0o700);env.update(TMPDIR=str(temporary),TMP=str(temporary),TEMP=str(temporary))
    request=dict(control['assertion'],mutation=control['mutation'])
    request_file=folder/'assertion-request.json';request_file.write_text(json.dumps(request,indent=2)+'\n')
    resolved_file=folder/'assertion-sites.json'
    # Input redirection is structured; no shell command construction is used.
    with request_file.open('rb') as stdin,resolved_file.open('xb') as stdout:
        subprocess.run([str(tools/'assertion-sites'),'-root',str(tree)],input=stdin.read(),stdout=stdout,env=env,check=True,timeout=30)
    resolved=source.read_json(resolved_file)
    require(source.source_fingerprint(tree)==initial,'assertion attribution changed source')
    mutation=control['mutation'];target=tree/mutation['path'];original=target.read_bytes();mode=target.stat().st_mode&0o777
    require(resolved['mutation']['source_sha256']==sha(original) and resolved['mutation']['symbol']==mutation['symbol'] and
            resolved['mutation']['file']==mutation['path'] and resolved['mutation']['offset']==original.index(mutation['old'].encode()),
            'mutation binding differs from current source')
    before=actual_files(tree,control);changed=dict(before)
    require(mutation['old'] and before[mutation['path']].count(mutation['old'])==1 and mutation['required_occurrences']==1,
            'mutation preimage differs')
    changed[mutation['path']]=before[mutation['path']].replace(mutation['old'],mutation['new'])
    phases={}
    try:
        phases['good']=phase(tree,folder/'good',control,paths,tools,before,False)
        target.write_bytes(changed[mutation['path']].encode())
        phases['intended']=phase(tree,folder/'intended',control,paths,tools,changed,True)
    finally:
        target.write_bytes(original)
    require(source.source_fingerprint(tree)==initial and not source.git(tree,'status','--porcelain','--untracked-files=all'),
            'restoration is not exact')
    phases['restored']=phase(tree,folder/'restored',control,paths,tools,before,False)
    context=dict(mutation=mutation,phases=phases,source_mode_before=oct(mode),source_mode_restored=oct(target.stat().st_mode&0o777))
    proof=validate(context,control['selection'],control['assertion'],resolved,control['output_fragment'])
    record=dict(id=identity,kind=control['kind'],head=head,source_fingerprint=initial,context=context,attribution=resolved,result=proof)
    (folder/'result.json').write_text(json.dumps(record,indent=2)+'\n')
    return dict(id=identity,passed=True,kind=control['kind'],result_sha256=source.digest(folder/'result.json'),
                mutation_path=mutation['path'],mutant_file_sha256=sha(changed[mutation['path']].encode()),
                compiled_mutant_sha256=phases['intended']['build']['binary_sha256'])


def relevant(paths):
    return any(name in ['go.mod','go.sum','Makefile','.goreleaser.yaml','.golangci.yml'] or
        name.startswith(('.github/workflows/','scripts/','cmd/','internal/','packaging/','completions/')) for name in paths)


def execute(options):
    repo,output,tools=options.repo.resolve(),options.output.resolve(),options.tools.resolve()
    base,head=source.revisions(repo,options.base,options.head,options.event)
    output.mkdir(mode=0o700);options.owns_output=True
    controls=source.read_json(HERE/'controls.json')
    validate_controls(controls)
    plan_path='scripts/fault-controls/controls.json'
    inherited=source.git(repo,'ls-tree','--name-only',base,'--',plan_path)
    inherited_ids=[]
    if inherited:
        require(inherited==plan_path,'baseline control path is ambiguous')
        old_file=output/'baseline-controls.json'
        old_file.write_text(source.git(repo,'show',base+':'+plan_path)+'\n')
        previous=source.read_json(old_file);validate_controls(previous)
        inherited_ids=[c['id'] for c in previous]
        require(set(inherited_ids)<={c['id'] for c in controls},'baseline required controls were removed')
    changed=source.git(repo,'diff','--name-only',base,head).splitlines()
    affected=relevant(changed)
    proof=dict(base=base,head=head,changed_paths=changed,affected=affected,controls=[],baseline_control_ids=inherited_ids,
               plan_sha256=source.digest(HERE/'controls.json'),scripts_sha256={n:source.digest(HERE/n) for n in ['run.py','oracle.py']})
    if affected:
        go=source.shutil.which('go');require(go is not None,'Go is unavailable')
        paths=source.go_paths(str(Path(go).resolve()))
        require(paths['GOOS'] in ['darwin','linux'],'focused control jobs must run natively on Darwin/Linux')
        proof['platform']={k:paths[k] for k in ['GOOS','GOARCH','CGO_ENABLED','GOVERSION']}
        proof['go_tool_sha256']=source.digest(Path(paths['GOROOT'])/'bin/go')
        proof['assertion_tool_sha256']=source.digest(tools/'assertion-sites')
        # Warm HEAD's modules once before offline builds in disposable worktrees.
        env=source.environment(paths,output/'download-home',output/'unused-cache',tools/'golangci-lint',output/'unused.json')
        env.update(GOPROXY='https://proxy.golang.org',GOSUMDB='sum.golang.org')
        before=source.source_fingerprint(repo)
        proof['modules']=command([str(Path(paths['GOROOT'])/'bin/go'),'mod','download'],repo,env,output/'modules.log',0,timeout=300)
        require(source.source_fingerprint(repo)==before,'module preparation changed checkout source')
        for control in controls:proof['controls'].append(run_control(repo,head,control,output,paths,tools))
        require(source.digest(tools/'assertion-sites')==proof['assertion_tool_sha256'],'assertion tool changed during controls')
    proof.update(passed=True,executed_controls=len(proof['controls']),
        intended_assertion_selections=len(proof['controls']),
        unique_source_mutants=len({(c['mutation_path'],c['mutant_file_sha256']) for c in proof['controls']}),
        unique_compiled_mutants=len({c['compiled_mutant_sha256'] for c in proof['controls']}))
    (output/'result.json').write_text(json.dumps(proof,indent=2)+'\n')
    print(json.dumps(dict(passed=True,affected=affected,executed_controls=proof['executed_controls'])))
    return 0


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--repo',type=Path,required=True);parser.add_argument('--base',required=True)
    parser.add_argument('--head',required=True);parser.add_argument('--event',required=True)
    parser.add_argument('--tools',type=Path,required=True);parser.add_argument('--output',type=Path,required=True)
    options=parser.parse_args()
    try:return execute(options)
    except (OSError,ValueError,KeyError,TypeError,subprocess.SubprocessError) as error:
        if getattr(options,'owns_output',False):(options.output/'failure.json').write_text(json.dumps(dict(passed=False,error=str(error)))+'\n')
        print('focused controls failed: '+str(error),file=sys.stderr);return 2


if __name__=='__main__':sys.exit(main())
