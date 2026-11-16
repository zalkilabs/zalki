"""Pure 141f event-state regressions; resolver values are explicitly synthetic."""
import copy
import hashlib
import json
import unittest
import oracle

PKG='example.test/fixture'
ROOT='TestRoot'
TEST='fixture_test.go'
SOURCE='package fixture\nimport "testing"\nfunc TestRoot(t *testing.T) {\n t.Fatal("PRIMARY_MARKER")\n t.Fatal("SECONDARY_MARKER")\n}\n'


def identity(marker):
    return dict(file=TEST,assertion_function=ROOT,fatal_literal=marker,fatal_call='t.Fatal("'+marker+'")',
                expected_assertion_sites=1,expected_file_assertion_sites=1,call_chain=[],
                require_helper_markers_in_callees=False,callback_direct=False)


def attestation(marker):
    return dict(file=TEST,source_sha256=hashlib.sha256(SOURCE.encode()).hexdigest(),function=ROOT,literal=marker,
                reviewed_call=identity(marker)['fatal_call'],assertion_sites=1,file_sites=1,
                report_lines=[4 if marker=='PRIMARY_MARKER' else 5],helper_chain_valid=True)


def event(action,name=None,text=None):
    value=dict(Action=action,Package=PKG)
    if name is not None:value['Test']=name
    if text is not None:value['Output']=text+'\n'
    return value


def stream(intended,secondary=False):
    values=[event('run',ROOT)]
    for child,marker,line in [('primary','PRIMARY_MARKER',4),('secondary','SECONDARY_MARKER',5)]:
        name=ROOT+'/'+child;failed=intended and (child=='primary' or secondary)
        values.append(event('run',name))
        if failed:values.append(event('output',name,TEST+':'+str(line)+': '+marker))
        values.append(event('fail' if failed else 'pass',name))
    values += [event('fail' if intended else 'pass',ROOT),event('fail' if intended else 'pass')]
    return values


def fixture(secondary=False):
    phases={}
    for name in ['good','intended','restored']:
        files={TEST:SOURCE,'guard.go':'package fixture\nfunc allowed() bool { return '+('true' if name=='intended' else 'false')+' }\n'}
        phases[name]=dict(build=dict(exit_code=0,compiled=True,binary_sha256=('b' if name=='intended' else 'a')*64,setup_ok=True),
            process_outcome=dict(exit_code=1 if name=='intended' else 0,timed_out=False,signalled=False,streams_complete=True,setup_ok=True),
            source_files=files,source_digest=oracle.source_digest(files),test2json_ndjson='\n'.join(map(json.dumps,stream(name=='intended',secondary))))
    return dict(mutation=dict(path='guard.go',old='return false',new='return true',required_occurrences=1),phases=phases,
                source_mode_before='0o644',source_mode_restored='0o644')


def validate(context,secondary=()):
    return oracle.validate(context,dict(package=PKG,test=ROOT),identity('PRIMARY_MARKER'),attestation('PRIMARY_MARKER'),'PRIMARY_MARKER',secondary,functional_sites=bool(secondary))


class EventStates(unittest.TestCase):
    def edit(self,change,context=None):
        context=context or fixture();phase=context['phases']['intended'];rows=[json.loads(line) for line in phase['test2json_ndjson'].splitlines()]
        change(rows);phase['test2json_ndjson']='\n'.join(map(json.dumps,rows));return context

    def test_complete_children_and_exact_package_terminal_pass(self):
        self.assertTrue(validate(fixture())['passed'])

    def test_secondary_functional_site_must_be_attested_and_not_an_extra_control(self):
        context=fixture(True)
        with self.assertRaisesRegex(ValueError,'descendant lacks'):validate(context)
        secondary=[dict(assertion=identity('SECONDARY_MARKER'),attribution=attestation('SECONDARY_MARKER'))]
        proof=validate(context,secondary);self.assertTrue(proof['passed']);self.assertEqual(len(proof['matched']),1)
        self.assertEqual(proof['secondary_functional_matches'][0]['test'],ROOT+'/secondary')
        stale=copy.deepcopy(secondary);stale[0]['attribution']['source_sha256']='0'*64
        with self.assertRaisesRegex(ValueError,'actual test source'):validate(context,stale)
        with self.assertRaisesRegex(ValueError,'duplicate secondary'):validate(context,secondary+secondary)

    def test_formatted_siblings_at_the_same_attested_call_need_the_R7_policy(self):
        source='package fixture\nimport "testing"\nfunc TestRoot(t *testing.T) {\n for _, value := range []string{"primary","secondary"} { t.Run(value, func(t *testing.T) { t.Fatalf("%s: assertion failed", value) }) }\n}\n'
        context=fixture(True)
        for label,phase in context['phases'].items():
            phase['source_files'][TEST]=source;phase['source_digest']=oracle.source_digest(phase['source_files'])
            rows=[json.loads(line) for line in phase['test2json_ndjson'].splitlines()]
            for row in rows:
                if row['Action']=='output':row['Output']='fixture_test.go:4: '+('primary' if row['Test'].endswith('/primary') else 'secondary')+': assertion failed\n'
            phase['test2json_ndjson']='\n'.join(map(json.dumps,rows))
        selected=dict(package=PKG,test=ROOT);site=identity('PRIMARY_MARKER')
        site.update(fatal_literal='%s: assertion failed',fatal_call='t.Fatalf("%s: assertion failed", value)')
        resolved=attestation('PRIMARY_MARKER');resolved.update(source_sha256=hashlib.sha256(source.encode()).hexdigest(),literal=site['fatal_literal'],reviewed_call=site['fatal_call'])
        with self.assertRaisesRegex(ValueError,'descendant lacks'):
            oracle.validate(context,selected,site,resolved,'primary: assertion failed')
        proof=oracle.validate(context,selected,site,resolved,'primary: assertion failed',functional_sites=True)
        self.assertEqual(len(proof['matched']),1);self.assertEqual(len(proof['functional_site_matches']),2)
        with self.assertRaisesRegex(ValueError,'no intended source-bound assertion'):
            oracle.validate(context,selected,site,resolved,'missing named primary',functional_sites=True)

    def test_unregistered_failed_descendant_cannot_earn_a_kill(self):
        context=self.edit(lambda rows:[row.update(Test=ROOT+'/new') for row in rows if row.get('Test')==ROOT+'/primary'])
        with self.assertRaisesRegex(ValueError,'not registered'):validate(context)

    def test_named_output_must_be_inside_its_actual_node_interval(self):
        def before(rows):rows.insert(1,rows.pop(2))
        def after(rows):rows.insert(3,rows.pop(2))
        for change in [before,after]:
            with self.subTest(change=change),self.assertRaisesRegex(ValueError,'RUN/terminal interval'):
                validate(self.edit(change))

    def test_unique_mutant_RUN_and_terminal_are_required(self):
        changes=[lambda rows:rows.pop(0),lambda rows:rows.insert(1,event('run',ROOT)),
                 lambda rows:rows.insert(-1,event('fail',ROOT)),lambda rows:rows.insert(-1,event('run',ROOT+'/unfinished'))]
        for change in changes:
            with self.subTest(change=change),self.assertRaises(ValueError):validate(self.edit(change))

    def test_every_baseline_ancestor_of_a_functional_failure_must_fail(self):
        context=fixture();parent=ROOT+'/primary';leaf=parent+'/leaf'
        for label,phase in context['phases'].items():
            values=[json.loads(line) for line in phase['test2json_ndjson'].splitlines()]
            result=[]
            for row in values:
                if row.get('Test')==parent and row['Action']=='run':
                    result += [row,event('run',leaf)]
                elif row.get('Test')==parent and row['Action']=='output':
                    result.append(dict(row,Test=leaf))
                elif row.get('Test')==parent and row['Action'] in ['pass','fail']:
                    result += [event(row['Action'],leaf),event('pass',parent)]
                else:result.append(row)
            phase['test2json_ndjson']='\n'.join(map(json.dumps,result))
        with self.assertRaisesRegex(ValueError,'descendant lacks'):validate(context)

    def test_skip_and_conflicting_package_status_never_pass(self):
        for change in [lambda rows:rows.insert(-2,event('skip',ROOT+'/secondary')),
                       lambda rows:rows.append(event('pass')),
                       lambda rows:rows.append(event('output',ROOT,'fixture_test.go:4: PRIMARY_MARKER'))]:
            with self.subTest(change=change),self.assertRaises(ValueError):validate(self.edit(change))

    def test_unknown_secondary_or_nonfailing_marker_cannot_explain_a_failure(self):
        context=self.edit(lambda rows:[r.update(Output='fixture_test.go:99: SECONDARY_MARKER\n') for r in rows if 'SECONDARY_MARKER' in r.get('Output','')],fixture(True))
        secondary=[dict(assertion=identity('SECONDARY_MARKER'),attribution=attestation('SECONDARY_MARKER'))]
        with self.assertRaisesRegex(ValueError,'descendant lacks'):validate(context,secondary)
        context=self.edit(lambda rows:rows.insert(5,event('output',ROOT+'/secondary','fixture_test.go:4: PRIMARY_MARKER')))
        with self.assertRaises(ValueError):validate(context)
