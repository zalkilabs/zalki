"""Pure bundle schema/stream fixtures; synthetic attestations are not Go AST proof."""
import copy
import hashlib
import importlib.util
import json
import sys
from pathlib import Path
import unittest
import bundle_inputs as inputs
import execution
import oracle
import run
import test_cli_phases as cli_cases
import test_event_states as events

HERE=Path(__file__).resolve().parent


def literal_fixture():
    primary=inputs.literal_identity('ignored stop')
    secondary=inputs.literal_identity('signal death')
    source=(HERE.parents[1]/primary['file']).read_text()
    def attestation(identity):
        label=identity['literal_argument']['callback']
        report=25 if label=='ignored stop' else 22
        return dict(file=identity['file'],source_sha256=hashlib.sha256(source.encode()).hexdigest(),
            function=identity['assertion_function'],literal=identity['fatal_literal'],reviewed_call=identity['fatal_call'],
            assertion_sites=1,file_sites=1,assertion_lines=[61],report_lines=[report],helper_chain_valid=True,
            call_chain=identity['call_chain'],literal_argument_binding=dict(request=identity['literal_argument'],
                formal_names=['t','mode','receive','failure'],formal_types=['*testing.T','string','bool','string'],
                physical_fatal_sites=1,instantiated_argument_sites=1,registered_sibling_sites=2,
                source_fatal_lines=[61],caller_report_lines=[report],runtime_test=inputs.LITERAL_ROOT+'/'+label.replace(' ','_')))
    phases={}
    for name in ['good','intended','restored']:
        intended=name=='intended';rows=[events.event('run',inputs.LITERAL_ROOT)]
        for identity in [primary,secondary]:
            child=inputs.LITERAL_ROOT+'/'+identity['literal_argument']['callback'].replace(' ','_')
            rows.append(events.event('run',child))
            if intended:
                report=25 if child.endswith('ignored_stop') else 22
                rows.append(events.event('output',child,'host_disable_test.go:'+str(report)+': '+identity['fatal_literal']))
            rows.append(events.event('fail' if intended else 'pass',child))
        rows.extend([events.event('fail' if intended else 'pass',inputs.LITERAL_ROOT),events.event('fail' if intended else 'pass')])
        files={primary['file']:source,'guard.go':'return '+('true' if intended else 'false')}
        phases[name]=dict(build=dict(exit_code=0,compiled=True,binary_sha256=('b' if intended else 'a')*64,setup_ok=True),
            process_outcome=dict(exit_code=1 if intended else 0,timed_out=False,signalled=False,streams_complete=True,setup_ok=True),
            source_files=files,source_digest=oracle.source_digest(files),test2json_ndjson='\n'.join(map(json.dumps,rows)))
    context=dict(mutation=dict(path='guard.go',old='return false',new='return true',required_occurrences=1),
        phases=phases,source_mode_before='0o644',source_mode_restored='0o644')
    return context,primary,attestation(primary),[dict(assertion=secondary,attribution=attestation(secondary))]


def invoke_literal(fixture):
    context,primary,resolved,secondary=fixture
    return oracle.validate(context,dict(package=events.PKG,test=inputs.LITERAL_ROOT),primary,resolved,
                           primary['fatal_literal'],secondary,functional_sites=True)


class BundleInputs(unittest.TestCase):
    def test_runner_import_preserves_complete_discovery_and_fixture_origins(self):
        before=sys.path[:]
        spec=importlib.util.spec_from_file_location('focused_import_boundary',HERE/'run.py')
        isolated=importlib.util.module_from_spec(spec)
        spec.loader.exec_module(isolated)
        self.assertEqual(sys.path,before,'strict helper import contaminated discovery search order')
        self.assertEqual(Path(isolated.__file__).resolve(),(HERE/'run.py').resolve())
        self.assertEqual(Path(isolated.source.__file__).resolve(),(HERE.parent/'lint-delta/run.py').resolve())
        loader=unittest.TestLoader()
        suite=loader.discover(str(HERE),pattern='test_*.py')
        self.assertEqual(loader.errors,[])
        for name in ['test_run','test_profiles','test_bundle_inputs']:
            self.assertEqual(Path(sys.modules[name].__file__).resolve(),(HERE/(name+'.py')).resolve())
        pending=[suite];ids=[]
        while pending:
            item=pending.pop()
            if isinstance(item,unittest.TestSuite):pending.extend(item)
            else:ids.append(item.id())
        self.assertIn('test_run.FocusedConstruction.test_actual_good_intended_restored_construction_and_exact_source_restore',ids)
        self.assertIn(__name__+'.BundleInputs.test_two_literals_share_one_fatal_but_keep_their_distinct_runtime_nodes',ids)

    def test_all_original_controls_and_bundle26_are_schema_valid(self):
        controls=json.loads((HERE/'controls.json').read_text())
        run.validate_controls(controls)
        self.assertEqual(len(controls),70)
        self.assertEqual(len([c for c in controls if c['id'] in inputs.BUNDLE_ROOTS or c['id']=='Z70F6']),26)
        old=copy.deepcopy(controls[:44])
        for c in old:c.pop('observations',None)
        run.validate_controls(old,baseline=True)
        with self.assertRaisesRegex(ValueError,'observations'):run.validate_controls(old)
        for c in controls[44:]:
            self.assertTrue(execution.profile(c)['race'])
            self.assertEqual(execution.profile(c)['test_seconds'],45)
            self.assertEqual(execution.profile(c)['process_seconds'],120)
            self.assertEqual(execution.profile(c)['build_seconds'],300)

    def test_only_the_exact_Darwin_owned_cleanup_fixture_is_permitted(self):
        run.validate_controls([copy.deepcopy(inputs.FIXTURE)])
        changes=[lambda c:c.update(id='OTHER'),lambda c:c.update(native_os=['linux']),
            lambda c:c['mutation'].update(path='internal/procwatch/other_test.go'),
            lambda c:c['mutation'].update(symbol='other'),lambda c:c['mutation'].update(old='t.Cleanup(other)'),
            lambda c:c['selection'].update(test='TestProcwatchReadBounded/early_stop'),
            lambda c:c['assertion']['call_chain'][1].update(caller='TestProcwatchReadBounded'),
            lambda c:c.update(profile='bundle_functional')]
        for change in changes:
            control=copy.deepcopy(inputs.FIXTURE);change(control)
            with self.subTest(change=change),self.assertRaises(ValueError):run.validate_controls([control])

    def test_M1_keeps_both_exact_observations_and_real_CLI_control(self):
        control=next(c for c in json.loads((HERE/'controls.json').read_text()) if c['id']=='M1F1')
        changes=[lambda c:c.update(observations=c['observations'][:1]),lambda c:c.update(observations=c['observations'][::-1]),
            lambda c:c.update(rebuild_cli_for_mutant=False),lambda c:c['selection'].update(test='TestOther'),
            lambda c:c['observations'][0]['selection'].update(test='TestOther')]
        for change in changes:
            candidate=copy.deepcopy(control);change(candidate)
            with self.subTest(change=change),self.assertRaises(ValueError):run.validate_controls([candidate])

    def test_registered_source_chain_counts_are_not_flattened(self):
        fixture=copy.deepcopy(inputs.FIXTURE);identity=fixture['assertion']
        resolved=dict(call_chain=identity['call_chain'],named_callback_sites=2)
        inputs.require_chain_binding(identity,resolved)
        for edit in [lambda r:r.update(named_callback_sites=1),lambda r:r.pop('call_chain'),
                     lambda r:r.update(call_chain=r['call_chain'][:1])]:
            changed=copy.deepcopy(resolved);edit(changed)
            with self.assertRaises(ValueError):inputs.require_chain_binding(identity,changed)

    def test_two_literals_share_one_fatal_but_keep_their_distinct_runtime_nodes(self):
        proof=invoke_literal(literal_fixture())
        self.assertEqual(proof['matched'][0]['test'],inputs.LITERAL_ROOT+'/ignored_stop')
        self.assertEqual(proof['secondary_functional_matches'][0]['test'],inputs.LITERAL_ROOT+'/signal_death')

    def test_literal_echo_wrong_formal_ordinal_sibling_and_counts_refuse(self):
        changes=[lambda f:f[2]['literal_argument_binding']['request'].update(argument_index=2),
            lambda f:f[2]['literal_argument_binding'].update(formal_types=['*testing.T','string','bool','any']),
            lambda f:f[2]['literal_argument_binding'].update(runtime_test=inputs.LITERAL_ROOT+'/signal_death'),
            lambda f:f[2]['literal_argument_binding'].update(physical_fatal_sites=2),
            lambda f:f[2]['literal_argument_binding'].update(physical_fatal_sites=True),
            lambda f:f[1]['literal_argument'].update(expected_call_sites=True),
            lambda f:f[2]['literal_argument_binding'].update(instantiated_argument_sites=2),
            lambda f:f[2].update(report_lines=[22]),lambda f:f[2].update(assertion_lines=[60]),
            lambda f:f[2].update(call_chain=[])]
        for change in changes:
            value=literal_fixture();change(value)
            with self.subTest(change=change),self.assertRaises(ValueError):invoke_literal(value)

    def test_whole_root_cannot_drop_primary_ignore_unknown_leaf_or_misplace_output(self):
        for mode in ['drop-primary','swapped-sibling','wrong-line','wrong-message','outside-interval','unknown-leaf']:
            value=literal_fixture();phase=value[0]['phases']['intended'];rows=[json.loads(line) for line in phase['test2json_ndjson'].splitlines()]
            outputs=[row for row in rows if row['Action']=='output'];primary=outputs[0]
            if mode=='drop-primary':rows.remove(primary)
            elif mode=='swapped-sibling':primary['Test']=inputs.LITERAL_ROOT+'/signal_death'
            elif mode=='wrong-line':primary['Output']=primary['Output'].replace(':25:',':22:')
            elif mode=='wrong-message':primary['Output']=primary['Output'].rstrip('\n')+' extra suffix\n'
            elif mode=='outside-interval':rows.remove(primary);rows.insert(1,primary)
            else:
                child=inputs.LITERAL_ROOT+'/unknown';rows[-2:-2]=[events.event('run',child),events.event('fail',child)]
                for name in ['good','restored']:
                    good=value[0]['phases'][name];base=[json.loads(line) for line in good['test2json_ndjson'].splitlines()]
                    base[-2:-2]=[events.event('run',child),events.event('pass',child)];good['test2json_ndjson']='\n'.join(map(json.dumps,base))
            phase['test2json_ndjson']='\n'.join(map(json.dumps,rows))
            with self.subTest(mode=mode),self.assertRaises(ValueError):invoke_literal(value)

    def test_supplementary_phases_reuse_binary_source_CLI_and_profile(self):
        context=cli_cases.fixture();specs=[]
        for suffix in ['lease_held','log_failed']:
            specs.append(dict(id=suffix,selection=dict(package=events.PKG,test=events.ROOT),
                assertion=events.identity('PRIMARY_MARKER'),attribution=events.attestation('PRIMARY_MARKER'),output_fragment='PRIMARY_MARKER'))
        for phase in context['phases'].values():
            phase['process_outcome']['receipt']=dict(argv=['go','tool','test2json','same.test','-test.timeout=120s','-test.run=^TestRoot$'])
            phase['observations']=[dict(id=s['id'],selection=s['selection'],binary_sha256=phase['build']['binary_sha256'],
                source_digest=phase['source_digest'],cli_sha256=phase['cli']['binary_sha256'],
                process_outcome=copy.deepcopy(phase['process_outcome']),test2json_ndjson=phase['test2json_ndjson']) for s in specs]
        proofs=oracle.validate_observations(context,specs,True,True)
        self.assertEqual(len(proofs),2);self.assertTrue(all(p['compiled_binary_reused'] and not p['new_unique_control'] for p in proofs))
        changes=[lambda c:c['phases']['intended']['observations'].pop(),
            lambda c:c['phases']['intended']['observations'][0].update(binary_sha256='0'*64),
            lambda c:c['phases']['intended']['observations'][0].update(source_digest='0'*64),
            lambda c:c['phases']['intended']['observations'][0].update(cli_sha256='0'*64),
            lambda c:c['phases']['intended']['observations'][0]['process_outcome']['receipt']['argv'].insert(-1,'-test.timeout=999s')]
        for change in changes:
            candidate=copy.deepcopy(context);change(candidate)
            with self.subTest(change=change),self.assertRaises(ValueError):oracle.validate_observations(candidate,specs,True,True)

if __name__=='__main__':unittest.main()
