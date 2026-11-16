"""Attribute complete compiled mutation phases to source-bound named assertions."""
import hashlib
import json
from pathlib import PurePosixPath
import re


def require(ok,message):
    if not ok:raise ValueError(message)


def sha(raw):return hashlib.sha256(raw).hexdigest()


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
    tests={};package_status=[];outputs=[]
    for event in events:
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
            if action in ['pass','fail','skip']:package_status.append(action)
            continue
        require(type(name) is str,'invalid test name')
        relevant=descendant(name,selected)
        if not relevant:
            require(selected.startswith(name+'/'),'test2json contains an unselected sibling or prefix lookalike')
            continue
        states=tests.setdefault(name,[])
        if action=='run':
            require(not states,'duplicate or late selected RUN');states.append('run')
        elif action in ['pass','fail','skip']:
            require(states==['run'],'selected terminal lacks a unique RUN');states.append(action)
        elif action=='output':outputs.append((name,event.get('Output','')))
    required='fail' if intended else 'pass'
    require(tests.get(selected)==['run',required],'exact selected RUN/outcome is missing')
    require(package_status==[required],'package outcome differs from intended selected result')
    require(all(v==['run',required] or (intended and v==['run','pass']) for v in tests.values()),
            'selected descendant skipped, unfinished or inconsistent')
    return outputs,tests


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


def validate(context,selected,assertion,resolved,output_fragment):
    """resolved must be produced from current source by versioned assertion-sites."""
    phases=validate_sources(context)
    file=assertion['file'];source=phases['good']['source_files'][file]
    require(resolved['file']==file and resolved['source_sha256']==sha(source.encode()) and
            resolved['function']==assertion['assertion_function'] and resolved['literal']==assertion['fatal_literal'] and
            resolved['reviewed_call']==assertion['fatal_call'],
            'AST attribution is not bound to the actual test source')
    require(resolved['file_sites']==assertion['expected_file_assertion_sites'] and
            resolved['assertion_sites']==assertion['expected_assertion_sites'],
            'AST assertion inventory differs')
    require(resolved['helper_chain_valid'] is True,'AST helper chain is not attested')
    if assertion.get('callback_direct') is True:
        require(resolved['callback_sites']==resolved['assertion_sites'] and assertion['call_chain'],
                'fresh callback attribution is not attested')
    allowed=resolved['report_lines']
    require(type(allowed) is list and allowed and all(type(n) is int and n>0 for n in allowed),
            'missing source-derived report positions')
    chain=assertion['call_chain']
    root=selected['test'].split('/')[0]
    require((chain[0]['caller'] if chain else assertion['assertion_function'])==root,
            'assertion caller is not the selected test root')
    selected_events(phases['good'],selected['package'],selected['test'],False)
    outputs,states=selected_events(phases['intended'],selected['package'],selected['test'],True)
    selected_events(phases['restored'],selected['package'],selected['test'],False)
    matched=[]
    for name,text in outputs:
        require(type(text) is str,'nontext test output')
        for line in text.splitlines():
            found=re.match(r'^\s*([^:\s]+):(\d+):\s?(.*)$',line)
            if found and found[1]==PurePosixPath(file).name and int(found[2]) in allowed and output_fragment in found[3]:
                require(states.get(name)==['run','fail'],'intended marker belongs to a nonfailing test')
                matched.append(dict(test=name,file=found[1],line=int(found[2])))
    require(matched,'no intended source-bound assertion in selected failing test/descendant')
    attributed={item['test'] for item in matched}
    require(all(state!=['run','fail'] or any(descendant(name,test) for name in attributed)
                for test,state in states.items()),'failing selected descendant lacks the intended assertion')
    return dict(passed=True,matched=matched,source_sha256=resolved['source_sha256'],
                phase_binary_sha256={k:v['build']['binary_sha256'] for k,v in phases.items()})
