"""Pure stream/receipt tests. Go AST generation is deliberately not mocked as proof."""
import hashlib
import json
from pathlib import Path
import unittest
import oracle

HERE=Path(__file__).resolve().parent
# These two require actual Go AST generation, covered by the Go source tests.
AST_CASES={'extra-third-rpc-marker-site','helper-marker-removed'}
REFUSALS={
    'wrong-package':'wrong package',
    'sibling-subtest':'unselected sibling or prefix',
    'prefix-lookalike':'unselected sibling or prefix',
    'missing-selected-run':'terminal lacks a unique RUN',
    'skipped-descendant':'descendant skipped',
    'timeout':'phase outcome',
    'compile-error':'did not compile',
    'setup-error':'phase outcome',
    'incomplete-stream':'phase outcome',
    'wrong-assertion':'no intended source-bound assertion',
    'wrong-reported-file':'no intended source-bound assertion',
    'wrong-caller-position':'no intended source-bound assertion',
    'selected-pass-instead-of-fail':'exact selected RUN/outcome',
    'package-pass-instead-of-fail':'package outcome',
    'missing-restored-phase':'phases are all required',
    'restoration-byte-mismatch':'restored source bytes differ',
    'restoration-mode-mismatch':'restored source mode differs',
    'bad-good-phase':'exact selected RUN/outcome',
    'non-json-event-stream':'Expecting value',
}


def resolved_fixture(case):
    """Synthetic resolver output for testing only the downstream stream checker."""
    identity=case['allowed_source_sites_and_caller_mapping']
    source=case['context']['phases']['good']['source_files'][identity['file']]
    return dict(file=identity['file'],source_sha256=hashlib.sha256(source.encode()).hexdigest(),
                function=identity['assertion_function'],literal=identity['fatal_literal'],reviewed_call=identity['fatal_call'],
                assertion_sites=identity['expected_assertion_sites'],file_sites=identity['expected_file_assertion_sites'],
                report_lines=[case['expected_event_assertion_positions']['synthetic_reported_line']],helper_chain_valid=True)


class MutationOracle(unittest.TestCase):
    def invoke(self,case,resolved=None):
        return oracle.validate(case['context'],case['selected'],case['allowed_source_sites_and_caller_mapping'],
                               resolved or resolved_fixture(case),case['allowed_source_sites_and_caller_mapping']['fatal_literal'])

    def test_complete_phase_and_selected_assertion_fixtures(self):
        rows=json.loads((HERE/'CASES.json').read_text())['cases']
        examined=[]
        for row in rows:
            if row['case_id'] in AST_CASES:continue
            with self.subTest(case=row['case_id']):
                case=json.loads((HERE/row['path']).read_text());examined.append(row['case_id'])
                if row['expected_accept']:self.assertTrue(self.invoke(case)['passed'])
                else:
                    with self.assertRaisesRegex(ValueError,REFUSALS[row['case_id']]):self.invoke(case)
        self.assertEqual(len(examined),24)

    def test_source_attestation_cannot_be_replayed_after_test_text_changes(self):
        case=json.loads((HERE/'fixtures/direct-named-assertion-positive.json').read_text())
        result=resolved_fixture(case);result['source_sha256']='0'*64
        with self.assertRaisesRegex(ValueError,'actual test source'):self.invoke(case,result)

    def test_source_inventory_and_ast_positions_are_both_required(self):
        case=json.loads((HERE/'fixtures/direct-named-assertion-positive.json').read_text())
        for edit in [lambda x:x.update(file_sites=2),lambda x:x.update(helper_chain_valid=False),lambda x:x.update(report_lines=[])]:
            with self.subTest(edit=edit):
                result=resolved_fixture(case);edit(result)
                with self.assertRaises(ValueError):self.invoke(case,result)

    def test_marker_is_not_credited_without_selected_and_package_failure(self):
        case=json.loads((HERE/'fixtures/direct-named-assertion-positive.json').read_text())
        phase=case['context']['phases']['intended']
        events=json.loads('['+','.join(phase['test2json_ndjson'].splitlines())+']')
        for event in events:
            if event.get('Test')=='TestSentinel/blue' and event['Action']=='fail':event['Action']='pass'
        phase['test2json_ndjson']='\n'.join(map(json.dumps,events))
        with self.assertRaises(ValueError):self.invoke(case)

    def test_valid_intended_marker_cannot_mask_race_panic_or_timeout_output(self):
        for marker in ['WARNING: DATA RACE','panic: unrelated','test timed out','[build failed]']:
            with self.subTest(marker=marker):
                case=json.loads((HERE/'fixtures/direct-named-assertion-positive.json').read_text())
                phase=case['context']['phases']['intended']
                events=[json.loads(line) for line in phase['test2json_ndjson'].splitlines()]
                events.insert(0,dict(Action='output',Package=case['selected']['package'],Output=marker+'\n'))
                phase['test2json_ndjson']='\n'.join(map(json.dumps,events))
                with self.assertRaisesRegex(ValueError,'diagnostic'):self.invoke(case)


if __name__=='__main__':unittest.main()
