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
# The strict helper needs its own sibling imports only while being loaded.
# Restore the caller's exact search path before importing focused fixtures.
_strict_import_path=sys.path[:]
try:
    sys.path.insert(0,str(HERE.parent/'lint-delta'))
    spec=importlib.util.spec_from_file_location('strict_source_runner',HERE.parent/'lint-delta/run.py')
    source=importlib.util.module_from_spec(spec);spec.loader.exec_module(source)
finally:
    sys.path[:]=_strict_import_path
from oracle import require,sha,source_digest,validate
from execution import profile,native_platforms,applicable
import cli
from bundle_inputs import validate_bundle_input, callback_prefix, chain_key, validate_literal_identity


def local_path(name):
    return type(name) is str and name and not PurePosixPath(name).is_absolute() and '\\' not in name and \
        '..' not in PurePosixPath(name).parts and name==PurePosixPath(name).as_posix()


def validate_assertion(assertion,selection):
    require(type(assertion) is dict and local_path(assertion['file']) and
            PurePosixPath(assertion['file']).parent.as_posix()==selection['directory'][2:] and
            assertion['file'].endswith('_test.go'),'assertion source differs from selected package')
    require(all(type(assertion.get(k)) is str and assertion[k] for k in
            ['assertion_function','fatal_literal','fatal_call']),'incomplete assertion identity')
    require(all(type(assertion.get(k)) is int and assertion[k]>0 for k in
            ['expected_assertion_sites','expected_file_assertion_sites']),'invalid assertion inventory')
    require(type(assertion.get('call_chain')) is list,'invalid assertion lineage')
    require(all(type(assertion.get(k,False)) is bool for k in ['require_helper_markers_in_callees','callback_direct']),
            'invalid assertion helper/callback mode')
    require(assertion['expected_file_assertion_sites']>=assertion['expected_assertion_sites'],'invalid assertion site bounds')
    for edge in assertion['call_chain']:
        require(type(edge) is dict and set(edge)<= {'caller','callee','caller_is_helper','kind','expected_call_sites'} and
                all(type(edge.get(k)) is str and edge[k] for k in ['caller','callee']) and
                type(edge.get('caller_is_helper')) is bool and edge.get('kind','helper') in ['helper','run_callback'],
                'invalid assertion lineage edge')
        require(type(edge.get('expected_call_sites',1)) is int and edge.get('expected_call_sites',1)>0,
                'invalid exact helper call-site count')
    callback_prefix(assertion['call_chain'])
    if assertion.get('literal_argument') is not None:
        validate_literal_identity(assertion)


def validate_access_field_mutation(control):
    mutation=control['mutation']
    if control['id']!='D1F2' and mutation.get('declaration_kind')!='type':return
    require(control['id']=='D1F2' and control['kind']=='production' and mutation==dict(
        path='internal/pluginfeed/plugin_access.go',symbol='PluginAccess',declaration_kind='type',
        old='type PluginAccess struct {',new='type PluginAccess struct {\n\tUnclassified bool `json:"unclassified,omitempty"`',
        required_occurrences=1),'unreviewed access field mutation')
    require(control['selection']==dict(package='github.com/zalkilabs/zalki/internal/pluginfeed',
        directory='./internal/pluginfeed',test='TestPluginAccessMessagesClassifiesEveryField'),
        'access field selection differs')
    require(control['assertion']==dict(file='internal/pluginfeed/plugin_access_messages_test.go',
        assertion_function='TestPluginAccessMessagesClassifiesEveryField',fatal_literal='UNLISTED_PLUGIN_ACCESS_FIELD: %s',
        fatal_call='t.Fatalf("UNLISTED_PLUGIN_ACCESS_FIELD: %s", field.Name)',expected_assertion_sites=1,
        expected_file_assertion_sites=1,call_chain=[],require_helper_markers_in_callees=False,callback_direct=False),
        'access field assertion differs')


def validate_controls(controls,baseline=False):
    require(type(controls) is list and controls and len({c['id'] for c in controls})==len(controls),'missing/duplicate controls')
    prefix='github.com/zalkilabs/zalki/'
    for c in controls:
        require(set(c)<= {'id','kind','mutation','selection','assertion','output_fragment','secondary_assertions',
                         'profile','native_os','rebuild_cli_for_mutant','observations'},'unknown control configuration')
        require(c['kind'] in ['production','runtime_config','fixture'] and re.fullmatch('[A-Z0-9-]+',c['id']), 'invalid control kind/ID')
        selection,mutation,assertion=c['selection'],c['mutation'],c['assertion']
        require(selection['directory'].startswith(('./internal/','./cmd/')) and
                local_path(selection['directory'][2:]) and selection['package']==prefix+selection['directory'][2:],
                'control package/directory differs')
        require(re.fullmatch(r'Test[A-Za-z0-9_]+(?:/[^\x00-\x1f/]+)*',selection['test']), 'invalid exact test selection')
        require(set(mutation)<= {'path','symbol','old','new','required_occurrences','declaration_kind'} and
                local_path(mutation['path']) and type(mutation['old']) is str and mutation['old'] and
                type(mutation['new']) is str and type(mutation['required_occurrences']) is int and mutation['required_occurrences']==1,
                'invalid exact mutation')
        kind=mutation.get('declaration_kind','function')
        require(kind in ['function','const','var','type'],'unknown mutation declaration kind')
        validate_access_field_mutation(c)
        if c['kind']=='runtime_config':
            require(mutation['path']=='.goreleaser.yaml' and mutation['symbol']=='builds[0].ldflags' and kind=='function',
                    'unreviewed runtime configuration target')
        elif c['kind']=='fixture':
            validate_bundle_input(c)
        else:
            require(mutation['path'].startswith(('internal/','cmd/')) and mutation['path'].endswith('.go') and
                    not mutation['path'].endswith('_test.go') and re.fullmatch('[A-Za-z_][A-Za-z0-9_]*',mutation['symbol']),
                    'mutation must target a named production declaration')
        validate_assertion(assertion,selection)
        require(type(c['output_fragment']) is str and c['output_fragment'],'missing intended output fragment')
        secondary=c.get('secondary_assertions',[])
        require(type(secondary) is list and (not secondary or profile(c)['functional_sites']),
                'secondary functional sites require their reviewed execution profile')
        seen={chain_key(assertion)}
        for identity in secondary:
            validate_assertion(identity,selection)
            require(re.match(r'^[A-Za-z_][A-Za-z0-9_]*\.(?:Fatal|Fatalf)\(',identity['fatal_call']) is not None,
                    'secondary functional site must be an exact Fatal call')
            key=chain_key(identity)
            require(key not in seen,'duplicate secondary functional assertion');seen.add(key)
        validate_bundle_input(c)
        if c['id']=='M1F1' and not baseline:
            require(c.get('observations'), 'both exact M1F1 observations are required')
        for observation in c.get('observations',[]):
            validate_assertion(observation['assertion'],observation['selection'])
        profile(c);native_platforms(c)


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
    names.update(s['file'] for s in control.get('secondary_assertions',[]))
    names.update(s['assertion']['file'] for s in control.get('observations',[]))
    return {name:(tree/name).read_bytes().decode('utf-8') for name in sorted(names)}


def phase(tree,folder,control,paths,tools,files,intended,cli_input=None):
    folder.mkdir()
    env=source.environment(paths,folder/'home',folder/'unused-cache',tools/'golangci-lint',folder/'unused.json')
    temporary=folder/'tmp';temporary.mkdir(mode=0o700);env.update(TMPDIR=str(temporary),TMP=str(temporary),TEMP=str(temporary))
    go=str(Path(paths['GOROOT'])/'bin/go')
    before=source.source_fingerprint(tree)
    selected_profile=profile(control)
    if cli_input is not None:
        cli_binary,cli_record=cli_input
        require(source.digest(cli_binary)==cli_record['binary_sha256'],'phase CLI changed before execution')
        env['ZALKI_TEST_BINARY']=str(cli_binary)
    binary=folder/'control.test'
    race='-race' if selected_profile['race'] else '-race=false'
    build=command([go,'test',race,'-p=1','-c','-o',str(binary),control['selection']['directory']],tree,env,folder/'build.log',0,
                  timeout=selected_profile['build_seconds'])
    require(source.source_fingerprint(tree)==before,'test compilation changed source')
    pattern='/'.join('^'+re.escape(part)+'$' for part in control['selection']['test'].split('/'))
    raw=folder/'test2json.log'
    argv=[go,'tool','test2json','-t','-p',control['selection']['package'],str(binary),'-test.v=test2json',
          '-test.timeout='+str(selected_profile['test_seconds'])+'s','-test.count=1','-test.run='+pattern]
    package=tree/control['selection']['directory'].removeprefix('./')
    execution=command(argv,package,env,raw,1 if intended else 0,timeout=selected_profile['process_seconds'])
    require(source.source_fingerprint(tree)==before,'focused execution changed source')
    require(actual_files(tree,control)==files,'phase source differs from recorded compile input')
    result=dict(build=dict(exit_code=0,compiled=True,binary_sha256=source.digest(binary),setup_ok=True,receipt=build),
        process_outcome=dict(exit_code=execution['exit'],timed_out=False,signalled=False,streams_complete=True,setup_ok=True,receipt=execution),
        source_files=files,source_digest=source_digest(files),test2json_ndjson=raw.read_text())
    if control.get('observations'):
        result['observations']=[]
        for index,observation in enumerate(control['observations']):
            selection=observation['selection']
            pattern='/'.join('^'+re.escape(part)+'$' for part in selection['test'].split('/'))
            observed_raw=folder/('observation-'+str(index)+'-test2json.log')
            observed_argv=argv[:-1]+['-test.run='+pattern]
            observed=command(observed_argv,package,env,observed_raw,1 if intended else 0,
                             timeout=selected_profile['process_seconds'])
            require(source.digest(binary)==result['build']['binary_sha256'],'supplementary binary identity changed')
            require(source.source_fingerprint(tree)==before and actual_files(tree,control)==files,
                    'supplementary execution changed source')
            result['observations'].append(dict(id=observation['id'],selection=selection,
                binary_sha256=result['build']['binary_sha256'],source_digest=result['source_digest'],
                cli_sha256=cli_record['binary_sha256'] if cli_input is not None else None,
                process_outcome=dict(exit_code=observed['exit'],timed_out=False,signalled=False,
                    streams_complete=True,setup_ok=True,receipt=observed),test2json_ndjson=observed_raw.read_text()))
    if cli_input is not None:
        require(source.digest(cli_binary)==cli_record['binary_sha256'],'phase CLI changed during execution')
        result.update(cli=cli_record,source_fingerprint=before)
    return result


def resolve_assertion(tree,folder,identity,mutation,tools,env,prefix):
    request=dict(identity,mutation=mutation)
    request_file=folder/(prefix+'-request.json');request_file.write_text(json.dumps(request,indent=2)+'\n')
    resolved_file=folder/(prefix+'-sites.json')
    with request_file.open('rb') as stdin,resolved_file.open('xb') as stdout:
        subprocess.run([str(tools/'assertion-sites'),'-root',str(tree)],input=stdin.read(),stdout=stdout,env=env,check=True,timeout=30)
    return source.read_json(resolved_file)


def run_control(repo,head,control,output,paths,tools):
    identity=control['id'];require(re.fullmatch('[A-Z0-9-]+',identity) is not None,'invalid control ID')
    folder=output/identity;folder.mkdir();tree=folder/'source';selected_profile=profile(control)
    if selected_profile['cli']:cli.clone_source(repo,head,tree,source)
    else:subprocess.run(['git','-C',str(repo),'worktree','add','--detach',str(tree),head],check=True)
    require(source.git(tree,'rev-parse','HEAD')==head,'fault source HEAD differs')
    initial=source.source_fingerprint(tree)
    env=source.environment(paths,folder/'home',folder/'unused-lint-cache',tools/'golangci-lint',folder/'unused.json')
    temporary=folder/'tmp';temporary.mkdir(mode=0o700);env.update(TMPDIR=str(temporary),TMP=str(temporary),TEMP=str(temporary))
    resolved=resolve_assertion(tree,folder,control['assertion'],control['mutation'],tools,env,'assertion')
    secondary=[]
    for index,site in enumerate(control.get('secondary_assertions',[])):
        secondary.append(dict(assertion=site,attribution=resolve_assertion(tree,folder,site,control['mutation'],tools,env,'secondary-'+str(index))))
    observations=[]
    for index,item in enumerate(control.get('observations',[])):
        attribution=resolve_assertion(tree,folder,item['assertion'],control['mutation'],tools,env,
                                      'observation-'+str(index)+'-assertion')
        require(attribution['mutation']==resolved['mutation'],'supplementary mutation attribution differs')
        observations.append(dict(item,attribution=attribution))
    require(source.source_fingerprint(tree)==initial,'assertion attribution changed source')
    mutation=control['mutation'];target=tree/mutation['path'];original=target.read_bytes();mode=target.stat().st_mode&0o777
    require(resolved['mutation']['source_sha256']==sha(original) and resolved['mutation']['symbol']==mutation['symbol'] and
            resolved['mutation']['file']==mutation['path'] and resolved['mutation']['offset']==original.index(mutation['old'].encode()) and
            resolved['mutation'].get('declaration_kind','function')==mutation.get('declaration_kind','function'),
            'mutation binding differs from current source')
    before=actual_files(tree,control);changed=dict(before)
    require(mutation['old'] and before[mutation['path']].count(mutation['old'])==1 and mutation['required_occurrences']==1,
            'mutation preimage differs')
    changed[mutation['path']]=before[mutation['path']].replace(mutation['old'],mutation['new'])
    baseline_cli=cli.build(tree,head,paths,folder/'baseline-cli',source,command,timeout=selected_profile['build_seconds']) if selected_profile['cli'] else None
    phases={}
    try:
        phases['good']=phase(tree,folder/'good',control,paths,tools,before,False,baseline_cli)
        target.write_bytes(changed[mutation['path']].encode())
        intended_cli=baseline_cli
        if control.get('rebuild_cli_for_mutant',False):
            intended_cli=cli.build(tree,head,paths,folder/'mutant-cli',source,command,modified=True,timeout=selected_profile['build_seconds'])
        phases['intended']=phase(tree,folder/'intended',control,paths,tools,changed,True,intended_cli)
    finally:
        target.write_bytes(original)
    require(source.source_fingerprint(tree)==initial and not source.git(tree,'status','--porcelain','--untracked-files=all'),
            'restoration is not exact')
    restored_resolution=resolve_assertion(tree,folder,control['assertion'],control['mutation'],tools,env,'restored-assertion')
    require(restored_resolution==resolved,'restored primary source attribution differs')
    for index,site in enumerate(secondary):
        again=resolve_assertion(tree,folder,site['assertion'],control['mutation'],tools,env,'restored-secondary-'+str(index))
        require(again==site['attribution'],'restored secondary source attribution differs')
    for index,item in enumerate(observations):
        again=resolve_assertion(tree,folder,item['assertion'],control['mutation'],tools,env,'restored-observation-'+str(index))
        require(again==item['attribution'],'restored observation source attribution differs')
    require(source.source_fingerprint(tree)==initial,'restored attribution changed source')
    phases['restored']=phase(tree,folder/'restored',control,paths,tools,before,False,baseline_cli)
    context=dict(mutation=mutation,phases=phases,source_mode_before=oct(mode),source_mode_restored=oct(target.stat().st_mode&0o777))
    if selected_profile['cli']:
        context['cli_policy']=dict(head=head,platform={k:paths[k] for k in ['GOVERSION','GOOS','GOARCH','CGO_ENABLED']},
                                   baseline_fingerprint=initial,rebuild_mutant=control.get('rebuild_cli_for_mutant',False))
    proof=validate(context,control['selection'],control['assertion'],resolved,control['output_fragment'],secondary,cli_required=selected_profile['cli'],
                   mutant_cli_required=control.get('rebuild_cli_for_mutant',False),functional_sites=selected_profile['functional_sites'],observations=observations)
    record=dict(id=identity,kind=control['kind'],head=head,source_fingerprint=initial,context=context,attribution=resolved,secondary=secondary,observations=observations,restored_attribution=restored_resolution,result=proof)
    (folder/'result.json').write_text(json.dumps(record,indent=2)+'\n')
    return dict(id=identity,passed=True,kind=control['kind'],result_sha256=source.digest(folder/'result.json'),
                mutation_path=mutation['path'],mutant_file_sha256=sha(changed[mutation['path']].encode()),
                compiled_mutant_sha256=phases['intended']['build']['binary_sha256'],
                mutant_CLI_sha256=phases['intended']['cli']['binary_sha256'] if selected_profile['cli'] else None)


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
        previous=source.read_json(old_file);validate_controls(previous,baseline=True)
        inherited_ids=[c['id'] for c in previous]
        require(set(inherited_ids)<={c['id'] for c in controls},'baseline required controls were removed')
    changed=source.git(repo,'diff','--name-only',base,head).splitlines()
    affected=relevant(changed)
    proof=dict(base=base,head=head,changed_paths=changed,affected=affected,controls=[],baseline_control_ids=inherited_ids,
               plan_sha256=source.digest(HERE/'controls.json'),scripts_sha256={n:source.digest(HERE/n) for n in ['run.py','oracle.py','checks.py','execution.py','cli.py','bundle_inputs.py']},not_applicable_controls=[])
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
        for control in controls:
            if applicable(control,paths['GOOS']):proof['controls'].append(run_control(repo,head,control,output,paths,tools))
            else:proof['not_applicable_controls'].append(dict(id=control['id'],native_os=native_platforms(control),actual_os=paths['GOOS'],reason='declared_native_applicability'))
        require(len(proof['controls'])+len(proof['not_applicable_controls'])==len(controls),'native control accounting differs')
        require(source.digest(tools/'assertion-sites')==proof['assertion_tool_sha256'],'assertion tool changed during controls')
    proof.update(passed=True,executed_controls=len(proof['controls']),
        intended_assertion_selections=len(proof['controls']),
        unique_source_mutants=len({(c['mutation_path'],c['mutant_file_sha256']) for c in proof['controls']}),
        unique_compiled_mutants=len({c['compiled_mutant_sha256'] for c in proof['controls']}),
        unique_execution_mutants=len({(c['compiled_mutant_sha256'],c['mutant_CLI_sha256']) for c in proof['controls']}))
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
