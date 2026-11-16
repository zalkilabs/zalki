"""Closed bundle input boundaries; no command, platform or privilege fallback."""
import json
from checks import require

FIXTURE = {'id': 'Z70F6', 'kind': 'fixture', 'mutation': {'path': 'internal/procwatch/process_read_darwin_test.go', 'symbol': 'startOwned', 'old': '\tt.Cleanup(func() { _ = c.cmd.Process.Kill(); _ = c.wait() })\n', 'new': '', 'required_occurrences': 1}, 'selection': {'package': 'github.com/zalkilabs/zalki/internal/procwatch', 'directory': './internal/procwatch', 'test': 'TestProcwatchReadBounded'}, 'assertion': {'file': 'internal/procwatch/process_read_darwin_test.go', 'assertion_function': 'checkEarlyStopJoined', 'fatal_literal': 'a started child was not joined after an early stop', 'fatal_call': 't.Fatal("a started child was not joined after an early stop")', 'expected_assertion_sites': 1, 'expected_file_assertion_sites': 1, 'call_chain': [{'caller': 'TestProcwatchReadBounded', 'callee': 'checkReadRealChildren', 'caller_is_helper': False, 'kind': 'run_callback'}, {'caller': 'checkReadRealChildren', 'callee': 'checkEarlyStopJoined', 'caller_is_helper': False, 'kind': 'run_callback'}], 'require_helper_markers_in_callees': False, 'callback_direct': False}, 'output_fragment': 'a started child was not joined after an early stop', 'profile': 'default', 'native_os': ['darwin']}
M1_OBSERVATIONS = [{'id': 'pending-lease-held', 'selection': {'package': 'github.com/zalkilabs/zalki/cmd/zalki', 'directory': './cmd/zalki', 'test': 'TestPluginSupervisorUnknownSnapshotAtExitIsNotACrash/then_waiting/lease_held'}, 'assertion': {'file': 'cmd/zalki/plugin_supervise_pending_test.go', 'assertion_function': 'checkPendingWaitingReasons', 'fatal_literal': 'a readable pending wait was not an uncounted wait: %q %q %d pending %q', 'fatal_call': 't.Fatalf("a readable pending wait was not an uncounted wait: %q %q %d pending %q", p.run.State, p.run.ReasonCode, len(p.run.Restarts), p.pendingEnd)', 'expected_assertion_sites': 1, 'expected_file_assertion_sites': 1, 'call_chain': [{'caller': 'TestPluginSupervisorUnknownSnapshotAtExitIsNotACrash', 'callee': 'checkPendingWaitingReasons', 'caller_is_helper': False, 'kind': 'run_callback'}], 'require_helper_markers_in_callees': False, 'callback_direct': True}, 'output_fragment': 'a readable pending wait was not an uncounted wait'}, {'id': 'pending-log-failed', 'selection': {'package': 'github.com/zalkilabs/zalki/cmd/zalki', 'directory': './cmd/zalki', 'test': 'TestPluginSupervisorUnknownSnapshotAtExitIsNotACrash/then_waiting/log_failed'}, 'assertion': {'file': 'cmd/zalki/plugin_supervise_pending_test.go', 'assertion_function': 'checkPendingWaitingReasons', 'fatal_literal': 'a readable pending wait was not an uncounted wait: %q %q %d pending %q', 'fatal_call': 't.Fatalf("a readable pending wait was not an uncounted wait: %q %q %d pending %q", p.run.State, p.run.ReasonCode, len(p.run.Restarts), p.pendingEnd)', 'expected_assertion_sites': 1, 'expected_file_assertion_sites': 1, 'call_chain': [{'caller': 'TestPluginSupervisorUnknownSnapshotAtExitIsNotACrash', 'callee': 'checkPendingWaitingReasons', 'caller_is_helper': False, 'kind': 'run_callback'}], 'require_helper_markers_in_callees': False, 'callback_direct': True}, 'output_fragment': 'a readable pending wait was not an uncounted wait'}]


def chain_key(assertion):
    return (assertion['file'], assertion['assertion_function'], assertion['fatal_call'], assertion['fatal_literal'],
            json.dumps(assertion.get('literal_argument'), sort_keys=True, separators=(',', ':')),
            json.dumps(assertion['call_chain'], sort_keys=True, separators=(',', ':')))


def callback_prefix(chain):
    count = 0
    for edge in chain:
        if edge.get('kind', 'helper') != 'run_callback':
            break
        require(edge.get('expected_call_sites',1)==1, 'named callback registration count must be one')
        count += 1
    require(all(e.get('kind', 'helper') == 'helper' for e in chain[count:]),
            'callback edges must be a connected registered prefix')
    if chain:
        require(not chain[0]['caller_is_helper'], 'caller chain must start at a non-helper test')
        names = [chain[0]['caller']]
        for index, edge in enumerate(chain):
            require(index == 0 or chain[index-1]['callee'] == edge['caller'], 'disconnected source chain')
            require(edge['callee'] not in names, 'cyclic source chain')
            names.append(edge['callee'])
    return count


def require_chain_binding(assertion, resolved):
    chain = assertion['call_chain']
    named = callback_prefix(chain)
    enhanced = named > 1 or (named and (len(chain) > named or assertion.get('callback_direct'))) or \
        any(edge.get('expected_call_sites', 1) != 1 for edge in chain)
    if enhanced or 'call_chain' in resolved:
        require(resolved.get('call_chain') == chain, 'AST source chain differs from requested path')
    if named:
        require(resolved.get('named_callback_sites') == named, 'named callback lineage is not attested')


def validate_bundle_input(control):
    if control['kind'] == 'fixture':
        require(control == FIXTURE, 'only the exact Darwin Z70F6 owned-start cleanup fixture is permitted')
    observations = control.get('observations', [])
    require(type(observations) is list, 'invalid supplementary observations')
    if observations:
        require(control['id'] == 'M1F1' and control['kind'] == 'production' and
                control['mutation']['path'] == 'cmd/zalki/plugin_supervise.go' and
                control['mutation']['symbol'] == 'classifyEnd' and
                control.get('profile') == 'r7_background' and control.get('rebuild_cli_for_mutant') is True and
                control['selection']['test'] == 'TestPluginSupervisorLeaseHeldIsNotACrash' and
                observations == M1_OBSERVATIONS,
                'M1F1 must retain its real CLI control and both exact pending-wait observations')
    if control.get('profile') == 'bundle_functional':
        require(control['id'] in BUNDLE_ROOTS and control['kind'] == 'production' and
                control['selection'] == BUNDLE_ROOTS[control['id']],
                'functional bundle profile is confined to exact original bundle whole roots')

BUNDLE_ROOTS = {'Z71F1': {'package': 'github.com/zalkilabs/zalki/internal/plugins', 'directory': './internal/plugins', 'test': 'TestHostDisableEndsQuietlyWhenPluginExitsNonZero'}, 'Z71F2': {'package': 'github.com/zalkilabs/zalki/internal/plugins', 'directory': './internal/plugins', 'test': 'TestHostDisableEndsQuietlyWhenPluginExitsNonZero'}, 'Z71F3': {'package': 'github.com/zalkilabs/zalki/internal/plugins', 'directory': './internal/plugins', 'test': 'TestHostDisableEndsQuietlyWhenPluginExitsNonZero'}, 'Z71F4': {'package': 'github.com/zalkilabs/zalki/internal/plugins', 'directory': './internal/plugins', 'test': 'TestHostDisableEndsQuietlyWhenPluginExitsNonZero'}, 'Z71F6': {'package': 'github.com/zalkilabs/zalki/internal/plugins', 'directory': './internal/plugins', 'test': 'TestHostDisableEndsQuietlyWhenPluginExitsNonZero'}, 'Z71F5': {'package': 'github.com/zalkilabs/zalki/internal/plugins', 'directory': './internal/plugins', 'test': 'TestHostDisableEndsQuietlyWhenPluginExitsNonZero'}, 'Z74H1': {'package': 'github.com/zalkilabs/zalki/cmd/zalki', 'directory': './cmd/zalki', 'test': 'TestPluginSupervisorUnknownSnapshotAtExitIsNotACrash'}, 'Z74H2': {'package': 'github.com/zalkilabs/zalki/cmd/zalki', 'directory': './cmd/zalki', 'test': 'TestPluginSupervisorUnknownSnapshotAtExitIsNotACrash'}, 'Z74H3': {'package': 'github.com/zalkilabs/zalki/cmd/zalki', 'directory': './cmd/zalki', 'test': 'TestPluginSupervisorUnknownSnapshotAtExitIsNotACrash'}, 'Z70F1': {'package': 'github.com/zalkilabs/zalki/internal/procwatch', 'directory': './internal/procwatch', 'test': 'TestProcwatchReadBounded'}, 'Z70F2': {'package': 'github.com/zalkilabs/zalki/internal/procwatch', 'directory': './internal/procwatch', 'test': 'TestProcwatchReadBounded'}, 'Z70F3': {'package': 'github.com/zalkilabs/zalki/internal/procwatch', 'directory': './internal/procwatch', 'test': 'TestProcwatchReadBounded'}, 'Z70F4': {'package': 'github.com/zalkilabs/zalki/internal/procwatch', 'directory': './internal/procwatch', 'test': 'TestProcwatchReadBounded'}, 'Z70F5': {'package': 'github.com/zalkilabs/zalki/internal/procwatch', 'directory': './internal/procwatch', 'test': 'TestProcwatchReadBounded'}, 'Z66G1': {'package': 'github.com/zalkilabs/zalki/cmd/zalki', 'directory': './cmd/zalki', 'test': 'TestPluginReinstallIgnoresLeftoverRunFiles'}, 'Z66G2': {'package': 'github.com/zalkilabs/zalki/cmd/zalki', 'directory': './cmd/zalki', 'test': 'TestPluginReinstallIgnoresLeftoverRunFiles'}, 'Z67K1': {'package': 'github.com/zalkilabs/zalki/cmd/zalki', 'directory': './cmd/zalki', 'test': 'TestPluginShowAndListPrintAddress'}, 'Z67K2': {'package': 'github.com/zalkilabs/zalki/cmd/zalki', 'directory': './cmd/zalki', 'test': 'TestPluginShowAndListPrintAddress'}, 'Z67K3': {'package': 'github.com/zalkilabs/zalki/cmd/zalki', 'directory': './cmd/zalki', 'test': 'TestPluginShowAndListPrintAddress'}, 'Z67K4': {'package': 'github.com/zalkilabs/zalki/cmd/zalki', 'directory': './cmd/zalki', 'test': 'TestPluginShowAndListPrintAddress'}, 'Z68G1': {'package': 'github.com/zalkilabs/zalki/cmd/zalki', 'directory': './cmd/zalki', 'test': 'TestPluginSupervisorForgetsRemovedNames'}, 'Z68G2': {'package': 'github.com/zalkilabs/zalki/cmd/zalki', 'directory': './cmd/zalki', 'test': 'TestPluginSupervisorForgetsRemovedNames'}, 'Z68G3': {'package': 'github.com/zalkilabs/zalki/cmd/zalki', 'directory': './cmd/zalki', 'test': 'TestPluginSupervisorForgetsRemovedNames'}, 'Z68G4': {'package': 'github.com/zalkilabs/zalki/cmd/zalki', 'directory': './cmd/zalki', 'test': 'TestPluginSupervisorForgetsRemovedNames'}, 'Z68G5': {'package': 'github.com/zalkilabs/zalki/cmd/zalki', 'directory': './cmd/zalki', 'test': 'TestPluginSupervisorForgetsRemovedNames'}}

LITERAL_SPECS = {
    'ignored stop': 'a plugin that ignored the stop was reported as clean',
    'signal death': 'a plugin that died by a signal at disable was reported as clean',
}
LITERAL_ROOT = 'TestHostDisableEndsQuietlyWhenPluginExitsNonZero'


def literal_identity(callback):
    return dict(file='internal/plugins/host_disable_test.go', assertion_function='checkLoudStop',
        fatal_literal=LITERAL_SPECS[callback], fatal_call='t.Fatal(failure)', expected_assertion_sites=1,
        expected_file_assertion_sites=1, call_chain=[dict(caller=LITERAL_ROOT,callee='checkLoudStop',
            caller_is_helper=False,kind='helper',expected_call_sites=2)],
        require_helper_markers_in_callees=True,callback_direct=False,
        literal_argument=dict(root=LITERAL_ROOT,callback=callback,parameter='failure',argument_index=3,
            parameter_type='string',test_argument='t',expected_call_sites=1))


def validate_literal_identity(assertion):
    argument=assertion.get('literal_argument')
    require(type(argument) is dict, 'literal binding must be an exact request object')
    require(all(type(argument.get(name)) is int for name in ['argument_index','expected_call_sites']),
            'literal argument ordinal/count must be integers')
    callback=argument.get('callback')
    require(callback in LITERAL_SPECS and assertion == literal_identity(callback),
            'only the exact two Z71F2 literal/formal identities are permitted')


def require_literal_binding(assertion, resolved):
    argument=assertion.get('literal_argument')
    if argument is None:
        require('literal_argument_binding' not in resolved,'unsolicited literal argument proof')
        return
    validate_literal_identity(assertion)
    callback=argument['callback']
    binding=resolved.get('literal_argument_binding',{})
    require(type(binding) is dict and all(type(binding.get(name)) is int for name in
            ['physical_fatal_sites','instantiated_argument_sites','registered_sibling_sites']),
            'literal physical/instantiated counts must be integers')
    expected=dict(request=argument,formal_names=['t','mode','receive','failure'],
        formal_types=['*testing.T','string','bool','string'],physical_fatal_sites=1,
        instantiated_argument_sites=1,registered_sibling_sites=2,
        source_fatal_lines=resolved.get('assertion_lines'),caller_report_lines=resolved.get('report_lines'),
        runtime_test=LITERAL_ROOT+'/'+callback.replace(' ','_'))
    require(resolved.get('literal_argument_binding')==expected and
            type(expected['source_fatal_lines']) is list and len(expected['source_fatal_lines'])==1 and
            all(type(n) is int and n>0 for n in expected['source_fatal_lines']) and
            type(expected['caller_report_lines']) is list and len(expected['caller_report_lines'])==1,
            'literal/formal binding echo or physical/instantiated inventory differs')
    require(resolved.get('call_chain')==assertion['call_chain'],'literal helper lineage is not attested')
