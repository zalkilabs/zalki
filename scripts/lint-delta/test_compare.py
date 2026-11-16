"""Pure report fixtures: no linter, Go toolchain or shared state required."""
import importlib.util
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

spec = importlib.util.spec_from_file_location('compare', Path(__file__).with_name('compare.py'))
compare = importlib.util.module_from_spec(spec)
spec.loader.exec_module(compare)


class StrictComparison(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.issue = dict(linter='funlen', file='internal/bus/message.go', scope='(*Store).Message',
                          text='Function is too long (81 > 80)', line=10, source_line='func example() {')

    def report(self, rows, name='input.json'):
        path = self.root / name
        path.write_text(json.dumps(rows))
        return compare.read_report(path)

    def test_line_motion_preserves_raw_function_key_and_multiplicity(self):
        moved = dict(self.issue, line=900)
        before = self.report([self.issue, self.issue], 'base.json')
        after = self.report([moved, moved], 'head.json')
        result = compare.compare(before, before, after, after)
        self.assertTrue(result['passed'])
        self.assertEqual(result['base_diagnostics'], 2)
        self.assertEqual(result['base_distinct_keys'], 1)

    def test_extra_same_key_is_a_regression_even_if_distinct_count_is_unchanged(self):
        base, head = self.report([self.issue], 'base'), self.report([self.issue, self.issue], 'head')
        result = compare.compare(base, base, head, head)
        self.assertFalse(result['passed'])
        self.assertEqual(result['added'], [dict(self.issue_key(), count=1)])

    def issue_key(self):
        return {key: self.issue[key] for key in ['linter', 'file', 'scope', 'text', 'source_line']}

    def test_literal_changes_are_not_normalized(self):
        base = self.report([self.issue], 'base')
        for field in ['linter', 'file', 'scope', 'text', 'source_line']:
            with self.subTest(field=field):
                changed = dict(self.issue); changed[field] += '_changed'
                head = self.report([changed], 'head')
                self.assertFalse(compare.compare(base, base, head, head)['passed'])

    def test_source_text_change_with_same_other_keys_and_count_requires_review(self):
        before = self.report([self.issue], 'base')
        changed = dict(self.issue, source_line='func replacement() {')
        after = self.report([changed], 'head')
        result = compare.compare(before, before, after, after)
        self.assertFalse(result['passed'])
        self.assertEqual(result['base_diagnostics'], result['head_diagnostics'])
        self.assertEqual(result['added'][0]['source_line'], changed['source_line'])

    def test_source_text_must_be_present_but_may_be_empty(self):
        for value in [None, 1, False]:
            with self.subTest(value=value):
                with self.assertRaises(ValueError): self.report([dict(self.issue, source_line=value)])
        missing = dict(self.issue); missing.pop('source_line')
        with self.assertRaises(ValueError): self.report([missing])
        self.assertEqual(sum(self.report([dict(self.issue, source_line='')]).values()), 1)

    def test_debt_removal_passes_without_a_numeric_budget(self):
        base, head = self.report([self.issue], 'base'), self.report([], 'head')
        result = compare.compare(base, base, head, head)
        self.assertTrue(result['passed'])
        self.assertEqual(result['removed'], [dict(self.issue_key(), count=1)])
        self.assertEqual(result['head_diagnostics'], 0)

    def test_either_tree_order_difference_fails_with_no_new_head_key(self):
        populated, empty = self.report([self.issue], 'full'), self.report([], 'empty')
        for inputs in [(populated, empty, empty, empty), (populated, populated, empty, populated)]:
            with self.subTest(inputs=inputs):
                result = compare.compare(*inputs)
                self.assertFalse(result['passed'])
                self.assertEqual(result['added'], [])

    def test_malformed_partial_and_ambiguous_input_cannot_be_empty_success(self):
        bad = ['', '[', '{}', 'null', '[{}]', '[{"linter":"x","linter":"y"}]', '[NaN]']
        for raw in bad:
            with self.subTest(raw=raw):
                path = self.root / 'bad'; path.write_text(raw)
                with self.assertRaises(ValueError): compare.read_report(path)
        for path in ['/outside/file.go', '../outside.go', 'a/../b.go', './a.go', 'a\\b.go']:
            with self.subTest(path=path):
                with self.assertRaises(ValueError): self.report([dict(self.issue, file=path)])

    def test_cli_exit_distinguishes_regression_and_invalid_input(self):
        paths = []
        for index in range(4):
            path = self.root / str(index); path.write_text(json.dumps([self.issue])); paths.append(path)
        command = [sys.executable, str(Path(__file__).with_name('compare.py'))]
        for flag, path in zip(['base-normal-first','base-strict-first','head-normal-first','head-strict-first'], paths):
            command += ['--' + flag, str(path)]
        def run(): return subprocess.run(command, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, timeout=10)
        self.assertEqual(run().returncode, 0)
        paths[2].write_text(json.dumps([self.issue, self.issue])); paths[3].write_bytes(paths[2].read_bytes())
        result = run(); self.assertEqual(result.returncode, 1); self.assertFalse(json.loads(result.stdout)['passed'])
        paths[0].write_text('['); result = run(); self.assertEqual(result.returncode, 2); self.assertEqual(result.stdout, '')


if __name__ == '__main__':
    unittest.main()
