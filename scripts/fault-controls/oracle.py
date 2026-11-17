"""Attribute complete compiled mutation phases to source-bound named assertions."""
import json
from pathlib import PurePosixPath
import re
from checks import require,sha
from bundle_inputs import chain_key, require_chain_binding, require_literal_binding



def source_digest(files):
    return sha(json.dumps(files,sort_keys=True).encode())


def descendant(name,selected):
    return name==selected or name.startswith(selected+'/')


def event_stream(raw):
    result=[]
    for line in raw.splitlines():
        require(line.strip(),'blank test2json record')
        event=json.loads(line)
        require(type(event) is dict and type(event.get('Action')) is str,'invalid test2json record')
        require(event['Action'] in ['start','run','pause','cont','output','pass','fail','skip','build-output','build-fail'],
                'unknown test2json action')
        result.append(event)
    require(result,'empty test2json stream')
    return result


def phase_status(phase,intended):
    build,outcome=phase['build'],phase['process_outcome']
    require(type(build.get('exit_code')) is int and build['exit_code']==0 and build.get('compiled') is True and build.get('setup_ok') is True,
            'phase did not compile successfully')
    require(type(build.get('binary_sha256')) is str and re.fullmatch('[0-9a-f]{64}',build['binary_sha256']),
            'missing compiled binary identity')
    require(type(outcome.get('exit_code')) is int and outcome['exit_code']==(1 if intended else 0) and outcome.get('setup_ok') is True and
            outcome.get('streams_complete') is True and outcome.get('timed_out') is False and outcome.get('signalled') is False,
            'phase outcome is not a complete intended assertion execution')
    require(phase['source_digest']==source_digest(phase['source_files']),'phase source identity differs')


def selected_events(phase,package,selected,intended):
    phase_status(phase,intended)
    raw=phase.get('raw_test2json_override',phase['test2json_ndjson'])
    events=event_stream(raw)
    tests={};package_status=[];outputs=[];observed=set();runs={};terminals={}
    for index,event in enumerate(events):
        action,name=event['Action'],event.get('Test')
        require(action not in ['build-fail','build-output'],'build diagnostics cannot kill a source mutation')
        if action=='output':
            text=event.get('Output')
            require(type(text) is str,'nontext test output')
            require(not any(marker in text for marker in ['WARNING: DATA RACE','panic:','fatal error:',
                    'test timed out','[setup failed]','[build failed]','flag provided but not defined']),
                    'runtime/build/setup diagnostic cannot kill a source mutation')
        require(event.get('Package')==package,'test2json contains the wrong package')
        if name is None:
            if action in ['pass','fail','skip']:
                package_status.append(action)
                require(all(len(v)==2 for v in tests.values()),'unfinished selected node at package terminal')
            continue
        require(type(name) is str,'invalid test name')
        relevant=descendant(name,selected)
        if not relevant:
            require(selected.startswith(name+'/'),'test2json contains an unselected sibling or prefix lookalike')
            continue
        require(not package_status,'selected event follows the package terminal')
        observed.add(name)
        if action=='run':
            require(name not in tests,'duplicate or late selected RUN')
            tests[name]=['run'];runs[name]=index
        elif action in ['pass','fail','skip']:
            require(tests.get(name)==['run'],'selected terminal lacks a unique RUN')
            tests[name].append(action);terminals[name]=index
        elif action=='output':outputs.append((name,event['Output'],index))
    required='fail' if intended else 'pass'
    require(tests.get(selected)==['run',required],'exact selected RUN/outcome is missing')
    require(package_status==[required],'package outcome differs from intended selected result')
    require(observed==set(tests),'observed selected node lacks complete RUN/terminal')
    require(all(v==['run',required] or (intended and v==['run','pass']) for v in tests.values()),
            'selected descendant skipped, unfinished or inconsistent')
    for name,text,index in outputs:
        require(runs[name]<index<terminals[name],'selected output is outside its RUN/terminal interval')
    return [(name,text) for name,text,index in outputs],tests


def validate_sources(context):
    phases=context['phases']
    require(set(phases)=={'good','intended','restored'},'good/intended/restored phases are all required')
    require(context.get('source_mode_before')==context.get('source_mode_restored') and
            context.get('source_mode_before') is not None,'restored source mode differs')
    good,bad,restored=[phases[x]['source_files'] for x in ['good','intended','restored']]
    require(good==restored,'restored source bytes differ')
    mutation=context['mutation'];path=mutation['path']
    require(PurePosixPath(path).is_absolute() is False and '..' not in PurePosixPath(path).parts,'mutation path escapes source')
    require(set(good)==set(bad) and path in good,'mutation source inventory differs')
    require(type(mutation['required_occurrences']) is int and mutation['required_occurrences']>0 and
            good[path].count(mutation['old'])==mutation['required_occurrences'] and mutation['old'],
            'mutation preimage differs from reviewed occurrence count')
    expected=dict(good);expected[path]=good[path].replace(mutation['old'],mutation['new'])
    require(expected==bad and good!=bad,'intended source is not the exact reviewed mutation')
    return phases


def attested_site(files,assertion,resolved,selected):
    file=assertion['file'];source=files[file]
    require(resolved['file']==file and resolved['source_sha256']==sha(source.encode()) and
            resolved['function']==assertion['assertion_function'] and resolved['literal']==assertion['fatal_literal'] and
            resolved['reviewed_call']==assertion['fatal_call'],
            'AST attribution is not bound to the actual test source')
    require(resolved['file_sites']==assertion['expected_file_assertion_sites'] and
            resolved['assertion_sites']==assertion['expected_assertion_sites'],'AST assertion inventory differs')
    require(resolved['helper_chain_valid'] is True,'AST helper chain is not attested')
    if assertion.get('callback_direct') is True:
        require(resolved['callback_sites']==resolved['assertion_sites'] and assertion['call_chain'],
                'fresh callback attribution is not attested')
    require_chain_binding(assertion,resolved)
    require_literal_binding(assertion,resolved)
    allowed=resolved['report_lines']
    require(type(allowed) is list and allowed and all(type(n) is int and n>0 for n in allowed),
            'missing source-derived report positions')
    chain=assertion['call_chain'];root=selected['test'].split('/')[0]
    require((chain[0]['caller'] if chain else assertion['assertion_function'])==root,
            'assertion caller is not the selected test root')
    return file,allowed


def assertion_matches(outputs,file,allowed,fragment=None):
    matched=[]
    for name,text in outputs:
        for line in text.splitlines():
            found=re.match(r'^\s*([^:\s]+):(\d+):\s?(.*)$',line)
            if found and found[1]==PurePosixPath(file).name and int(found[2]) in allowed and (fragment is None or fragment in found[3]):
                matched.append(dict(test=name,file=found[1],line=int(found[2])))
    return matched


def site_matches(outputs,file,allowed,identity,resolved,fragment=None):
    if identity.get('literal_argument') is None:
        return assertion_matches(outputs,file,allowed,fragment)
    literal=identity['fatal_literal']
    runtime=resolved['literal_argument_binding']['runtime_test']
    require(fragment is None or fragment==literal,'literal argument cannot use a fragment fallback')
    exact=[]
    for name,text in outputs:
        if name!=runtime:
            continue
        for line in text.splitlines():
            found=re.match(r'^\s*([^:\s]+):(\d+):\s?(.*)$',line)
            if found and found[1]==PurePosixPath(file).name and int(found[2]) in allowed and found[3]==literal:
                exact.append(dict(test=name,file=found[1],line=int(found[2])))
    return exact


def validate(context,selected,assertion,resolved,output_fragment,secondary=(),cli_required=False,mutant_cli_required=False,functional_sites=False,observations=()):
    """All resolved sites must come from the same current versioned Go resolver."""
    phases=validate_sources(context)
    file,allowed=attested_site(phases['good']['source_files'],assertion,resolved,selected)
    _,baseline=selected_events(phases['good'],selected['package'],selected['test'],False)
    outputs,states=selected_events(phases['intended'],selected['package'],selected['test'],True)
    selected_events(phases['restored'],selected['package'],selected['test'],False)
    failures={name for name,state in states.items() if state==['run','fail']}
    require(failures<=set(baseline),'mutant failure is not registered by the clean positive')
    matched=site_matches(outputs,file,allowed,assertion,resolved,output_fragment)
    require(matched,'no intended source-bound assertion in selected failing test/descendant')
    require(all(item['test'] in failures for item in matched),'intended marker belongs to a nonfailing test')
    require(not secondary or functional_sites,'secondary functional sites require their reviewed event policy')
    primary_functional=site_matches(outputs,file,allowed,assertion,resolved) if functional_sites else matched
    require(all(item['test'] in failures for item in primary_functional),'functional assertion belongs to a nonfailing test')
    functional={item['test'] for item in primary_functional};secondary_matched=[];seen=set()
    for site in secondary:
        identity=site['assertion'];attestation=site['attribution']
        require(re.match(r'^[A-Za-z_][A-Za-z0-9_]*\.(?:Fatal|Fatalf)\(',identity['fatal_call']) is not None,
                'secondary functional site must be an exact Fatal call')
        other,positions=attested_site(phases['good']['source_files'],identity,attestation,selected)
        key=chain_key(identity)
        require(key not in seen,'duplicate secondary functional assertion');seen.add(key)
        hits=site_matches(outputs,other,positions,identity,attestation)
        require(all(item['test'] in failures for item in hits),'secondary assertion belongs to a nonfailing test')
        secondary_matched.extend(hits);functional.update(item['test'] for item in hits)
    explained={test for test in baseline if any(descendant(name,test) for name in functional)}
    require(failures==explained,'failing selected descendant lacks the intended assertion')
    result=dict(passed=True,matched=matched,source_sha256=resolved['source_sha256'],
                phase_binary_sha256={k:v['build']['binary_sha256'] for k,v in phases.items()})
    if secondary:result['secondary_functional_matches']=secondary_matched
    if functional_sites:result['functional_site_matches']=primary_functional+secondary_matched
    require(not mutant_cli_required or cli_required,'mutant CLI requires a CLI execution profile')
    require(not cli_required or context.get('cli_policy') is not None,'required phase CLI policy is missing')
    if cli_required:
        require(context['cli_policy']['rebuild_mutant'] is mutant_cli_required,'phase CLI rebuild policy differs from recipe')
    if context.get('cli_policy') is not None:
        result['phase_CLI_sha256']=validate_cli_phases(context)
    if observations:
        result['observations']=validate_observations(context,observations,cli_required,mutant_cli_required)
    return result


def validate_cli_phases(context):
    from cli import verify
    policy=context['cli_policy']
    require(type(policy) is dict and set(policy)=={'head','platform','baseline_fingerprint','rebuild_mutant'},
            'invalid phase CLI policy')
    require(type(policy['rebuild_mutant']) is bool,'invalid mutant CLI policy')
    phases=context['phases'];baseline=None;hashes={}
    for name in ['good','intended','restored']:
        phase=phases[name];mutant=name=='intended' and policy['rebuild_mutant']
        fingerprint=phase.get('source_fingerprint') if mutant else policy['baseline_fingerprint']
        if mutant:require(fingerprint!=policy['baseline_fingerprint'],'mutant CLI source is still the clean baseline')
        verify(phase.get('cli'),policy['head'],policy['platform'],fingerprint,mutant)
        hashes[name]=phase['cli']['binary_sha256']
        if name=='good':baseline=hashes[name]
        elif not mutant:require(hashes[name]==baseline,'baseline/restored CLI identity changed')
    return hashes


def validate_observations(context,observations,cli_required,mutant_cli_required):
    result=[]
    for index,item in enumerate(observations):
        phases={}
        for name,phase in context['phases'].items():
            observed=phase.get('observations')
            require(type(observed) is list and len(observed)==len(observations),
                    'all supplementary observations must be present in every phase')
            record=observed[index]
            require(set(record)=={'id','selection','binary_sha256','source_digest','cli_sha256',
                    'process_outcome','test2json_ndjson'} and record['id']==item['id'] and
                    record['selection']==item['selection'],'supplementary selection differs')
            require(record['binary_sha256']==phase['build']['binary_sha256'] and
                    record['source_digest']==phase['source_digest'],'supplementary binary/source identity differs')
            require(record['cli_sha256']==(phase['cli']['binary_sha256'] if cli_required else None),
                    'supplementary CLI identity differs')
            pattern='/'.join('^'+re.escape(part)+'$' for part in item['selection']['test'].split('/'))
            original=phase['process_outcome']['receipt']['argv']
            require(record['process_outcome']['receipt']['argv']==original[:-1]+['-test.run='+pattern],
                    'supplementary execution must reuse the original binary and execution profile')
            phases[name]=dict(phase,process_outcome=record['process_outcome'],test2json_ndjson=record['test2json_ndjson'])
            phases[name].pop('observations',None)
            phases[name].pop('raw_test2json_override',None)
        observed_context=dict(context,phases=phases)
        proof=validate(observed_context,item['selection'],item['assertion'],item['attribution'],
                       item['output_fragment'],cli_required=cli_required,mutant_cli_required=mutant_cli_required)
        result.append(dict(id=item['id'],selection=item['selection'],result=proof,
                           compiled_binary_reused=True,new_unique_control=False))
    return result
