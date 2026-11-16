"""Fake compiler integration fixtures; never invokes a Go tool or binary."""
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

HERE = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location('platform_compile', HERE/'run.py')
runner = importlib.util.module_from_spec(spec)
spec.loader.exec_module(runner)

FAKE = r'''#!INTERPRETER
import json,os,sys
from pathlib import Path
t=Path(__file__).resolve().parent.parent;r=Path.cwd();args=sys.argv[1:]
mode=json.loads((t/'mode.json').read_text())
with (t/'calls.jsonl').open('a') as f:
 f.write(json.dumps(dict(argv=args,arch=os.environ.get('GOARCH'),cgo=os.environ.get('CGO_ENABLED'),
                        ambient_secret_present='PLATFORM_FIXTURE_SECRET' in os.environ))+'\n')
if args[0]=='env':
 values=dict(GOROOT=str(t),GOCACHE=str(t/'cache'),GOMODCACHE=str(t/'modules'),GOPATH=str(t/'path'),
             GOOS=os.environ.get('GOOS','linux'),GOARCH=os.environ.get('GOARCH','amd64'),
             CGO_ENABLED=os.environ.get('CGO_ENABLED','1'),GOVERSION='go1.26.11-fake')
 if mode=='profile-mismatch' and len(args[2:])==4:values['CGO_ENABLED']='1'
 print(json.dumps({k:values[k] for k in args[2:]}));sys.exit(0)
def row(d):
 rel=d.relative_to(r).as_posix();names=sorted(p.name for p in d.glob('*.go'))
 ignored=[n for n in names if (d/n).read_text().startswith('//go:build darwin')]
 tests=[n for n in names if n.endswith('_test.go') and n not in ignored]
 result=dict(Dir=str(d),ImportPath='example.test/compiler/'+rel,
             GoFiles=[n for n in names if n not in ignored and n not in tests],
             TestGoFiles=[n for n in tests if n!='external_test.go'],
             XTestGoFiles=[n for n in tests if n=='external_test.go'],IgnoredGoFiles=ignored)
 if ignored and not any(result[k] for k in ['GoFiles','TestGoFiles','XTestGoFiles']):
  result['Error']=dict(Err='build constraints exclude all Go files in '+str(d))
 return result
if args[0]=='list':
 rows=[row(d) for d in sorted({p.parent for p in r.rglob('*.go')})]
 if '-e' in args:rows=[row((r/args[-1]).resolve())]
 else:
  rows=[v for v in rows if 'Error' not in v]
  if mode in ['partial-success','partial-exit']:rows=rows[:1]
  if mode=='empty':rows=[]
  if mode=='duplicate':rows=rows+rows[:1]
  if mode=='package-error':rows[0]['Error']=dict(Err='missing dependency')
  if mode=='incomplete':rows[0]['Incomplete']=True
  if mode=='deps-error':rows[0]['DepsErrors']=[dict(Err='bad dependency')]
  if mode=='foreign':rows[0]['ImportPath']='foreign.test/wrong'
  if mode=='outside':rows[0]['Dir']=str(r.parent)
  if mode=='cgo-files':rows[0]['CgoFiles']=rows[0]['GoFiles']
  if mode=='no-tests':
   for v in rows:v['TestGoFiles']=v['XTestGoFiles']=[]
 if mode=='malformed':print('{');sys.exit(0)
 if mode=='duplicate-key':print('{"Dir":"a","Dir":"b"}');sys.exit(0)
 for v in rows:print(json.dumps(v))
 if mode=='stderr':print('discovery warning',file=sys.stderr)
 sys.exit(3 if mode=='partial-exit' else 0)
if args[0]=='vet' and mode=='vet-fail':print('vet failure',file=sys.stderr);sys.exit(4)
if args[0]=='build' and mode=='build-fail':sys.exit(5)
if args[0]=='vet' and mode=='arm64-fail' and os.environ['GOARCH']=='arm64':sys.exit(6)
if args[0]=='build':
 d=Path(args[args.index('-o')+1]);(d/'zalki').write_bytes(b'fake-linked-cli')
 if mode=='source-change':(r/'lib/p.go').write_text('package changed\n')
 if mode=='tool-change':
  with Path(__file__).open('a') as f:f.write('\n# changed compiler\n')
if args[0]=='test':
 assert '-c' in args,'runtime test execution forbidden'
 if mode=='test-fail':sys.exit(7)
 if mode!='missing-binary':
  Path(args[args.index('-o')+1]).write_bytes(b'' if mode=='empty-binary' else b'fake-test-binary')
'''


class CompileFixtures(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name).resolve()
        self.repo = self.root/'repo'
        self.repo.mkdir()
        (self.repo/'go.mod').write_text('module example.test/compiler\n')
        self.package('cmd/zalki')
        self.package('lib', external=True)
        self.tool = self.root/'tool'
        (self.tool/'bin').mkdir(parents=True)
        self.go = self.tool/'bin/go'
        self.go.write_text(FAKE.replace('INTERPRETER', sys.executable))
        self.go.chmod(0o755)
        (self.tool/'mode.json').write_text('"good"')

    def package(self, name, external=False):
        directory = self.repo/name
        directory.mkdir(parents=True)
        (directory/'p.go').write_text('package fixture\n')
        (directory/('external_test.go' if external else 'p_test.go')).write_text('package fixture\n')

    def run_mode(self, mode, name='out'):
        (self.tool/'mode.json').write_text(json.dumps(mode))
        def git(repo, *args):
            return '' if args[0] == 'status' else ('a'*40 if args[-1] == 'HEAD' else 'b'*40)
        with patch.object(runner.source, 'git', side_effect=git):
            return runner.execute(self.repo, self.root/name, str(self.go))

    def calls(self):
        return [json.loads(line) for line in (self.tool/'calls.jsonl').read_text().splitlines()]

    def test_success_external_tests_and_exact_two_profiles(self):
        with patch.dict(os.environ, PLATFORM_FIXTURE_SECRET='not-for-child'):
            result = self.run_mode('good')
        self.assertTrue(result['passed'])
        self.assertTrue(result['source_unchanged'])
        self.assertEqual(len(result['profiles']['amd64']['test_binaries']), 2)
        self.assertFalse(result['profiles']['arm64']['test_binaries'])
        calls = self.calls()
        self.assertFalse(any(c['ambient_secret_present'] for c in calls))
        self.assertTrue(all('-c' in c['argv'] for c in calls if c['argv'][0] == 'test'))
        self.assertFalse(any(c['argv'][0] == 'test' and c['arch'] == 'arm64' for c in calls))
        self.assertIn('./cmd/zalki', next(c['argv'] for c in calls if c['argv'][0] == 'vet' and c['arch'] == 'arm64'))

    def test_selection_grows_past_historical_fifteen(self):
        for index in range(15):
            self.package('extra'+str(index))
        self.assertEqual(len(self.run_mode('good')['profiles']['amd64']['test_binaries']), 17)

    def test_explicit_target_exclusion_is_recorded(self):
        directory = self.repo/'darwinonly'
        directory.mkdir()
        (directory/'p.go').write_text('//go:build darwin\npackage fixture\n')
        result = self.run_mode('good')
        self.assertEqual(result['profiles']['amd64']['inventory']['excluded'][0]['directory'], 'darwinonly')

    def test_bad_discovery_never_starts_compilation(self):
        modes = {'partial-exit':'compile command failed',
                 'partial-success':'wildcard discovery omitted a selected package',
                 'stderr':'discovery wrote stderr', 'empty':'empty discovery',
                 'malformed':'Expecting property name', 'duplicate-key':'duplicate discovery JSON key',
                 'duplicate':'duplicate discovered package', 'package-error':'package discovery contains an error',
                 'incomplete':'incomplete or empty package inventory', 'deps-error':'package discovery has dependency errors',
                 'foreign':'package import identity differs', 'outside':'package directory outside source',
                 'cgo-files':'CGO-disabled discovery selected CgoFiles',
                 'no-tests':'package file inventory omitted or duplicated current source'}
        for index, (mode, wanted) in enumerate(modes.items()):
            with self.subTest(mode=mode):
                previous = len(self.calls()) if (self.tool/'calls.jsonl').exists() else 0
                with self.assertRaisesRegex(ValueError, wanted):
                    self.run_mode(mode, 'bad-'+str(index))
                self.assertFalse(any(c['argv'][0] in ['vet','build','test'] for c in self.calls()[previous:]))
                self.assertFalse(json.loads((self.root/('bad-'+str(index))/'result.json').read_text())['passed'])

    def test_production_only_selection_does_not_green_empty_test_coverage(self):
        (self.repo/'cmd/zalki/p_test.go').unlink()
        (self.repo/'lib/external_test.go').unlink()
        with self.assertRaisesRegex(ValueError, 'empty package/test-bearing selection'):
            self.run_mode('good')
        self.assertFalse(any(c['argv'][0] in ['vet','build','test'] for c in self.calls()))

    def test_failed_commands_missing_outputs_and_identity_changes_refuse(self):
        modes = {'profile-mismatch':'actual compiler profile differs',
                 'vet-fail':'amd64/vet.receipt.json', 'build-fail':'amd64/build.receipt.json',
                 'test-fail':'amd64/test-0.receipt.json', 'missing-binary':'test compile output missing/invalid',
                 'empty-binary':'test compile output missing/invalid', 'arm64-fail':'arm64/vet.receipt.json',
                 'tool-change':'compiler binary changed'}
        for index, (mode, wanted) in enumerate(modes.items()):
            with self.subTest(mode=mode):
                with self.assertRaisesRegex(ValueError, wanted):
                    self.run_mode(mode, 'failure-'+str(index))
                result = json.loads((self.root/('failure-'+str(index))/'result.json').read_text())
                self.assertFalse(result['passed'])
        receipt = json.loads((self.root/'failure-1/amd64/vet.receipt.json').read_text())
        self.assertEqual(receipt['exit'], 4)
        self.assertEqual((self.root/'failure-1/amd64/vet.stderr').read_text(), 'vet failure\n')

    def test_source_mutation_is_not_a_pass(self):
        with self.assertRaisesRegex(ValueError, 'changed source'):
            self.run_mode('source-change')
        result = json.loads((self.root/'out/result.json').read_text())
        self.assertFalse(result['source_unchanged'])
        self.assertNotEqual(result['source_after'], result['source_fingerprint'])

    def test_timeout_receipt_is_actual_failure(self):
        folder = self.root/'timeout'
        folder.mkdir()
        env = dict(GOOS='linux', GOARCH='amd64', CGO_ENABLED='0')
        with self.assertRaisesRegex(ValueError, 'command failed'):
            runner.command([sys.executable,'-c','import time; time.sleep(2)'], self.repo, env, folder, 'sleep', timeout=0.05)
        receipt = json.loads((folder/'sleep.receipt.json').read_text())
        self.assertTrue(receipt['timed_out'])
        self.assertNotEqual(receipt['exit'], 0)


if __name__ == '__main__':
    unittest.main()
