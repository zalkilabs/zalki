"""Checkpoint failure fixtures use a fake SDK; no Go tool or client is run."""
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
spec = importlib.util.spec_from_file_location('checkpoint_runner', HERE/'run.py')
runner = importlib.util.module_from_spec(spec)
spec.loader.exec_module(runner)

FAKE = r'''#!PYTHON
import json,os,sys
from pathlib import Path
root=Path(__file__).resolve().parent.parent;cwd=Path.cwd();a=sys.argv[1:]
mode=(root/'mode').read_text();profile=dict(GOOS='darwin',GOARCH='arm64',CGO_ENABLED='1',GOVERSION='go1.26.11-fake')
with (root/'calls.jsonl').open('a') as f:f.write(json.dumps(dict(args=a,secret='CHECKPOINT_SECRET' in os.environ))+'\n')
if a[0]=='env':
 values=dict(profile,GOROOT=str(root),GOCACHE=str(root/'cache'),GOMODCACHE=str(root/'modules'),GOPATH=str(root/'path'))
 print(json.dumps({k:values[k] for k in a[2:]}));sys.exit(0)
if a[0]=='mod':sys.exit(0)
if a[0]=='list':
 dirs=sorted({p.parent for p in cwd.rglob('*.go')})
 if mode=='partial' and '-e' in a and a[-1]=='./...':dirs=dirs[:1]
 if a[-1]!='./...':dirs=[(cwd/a[-1]).resolve()]
 for d in dirs:
  files=sorted(p.name for p in d.glob('*.go'));rel=d.relative_to(cwd).as_posix()
  print(json.dumps(dict(Dir=str(d),ImportPath='github.com/zalkilabs/zalki/'+rel,
    GoFiles=[n for n in files if not n.endswith('_test.go')],TestGoFiles=[n for n in files if n.endswith('_test.go') ])))
 sys.exit(0)
if a[0]=='build':Path(a[a.index('-o')+1]).write_bytes(b'fake-cli-not-executable');sys.exit(0)
if a[0]=='version':
 head=(cwd/'.git/HEAD').read_text().strip()
 if mode=='wrong-cli' and not a[-1].endswith('arch-metrics'):head='0'*40
 values=dict(profile,**{'vcs.revision':head,'vcs.modified':'false'})
 settings=[dict(Key=k,Value=v) for k,v in values.items() if k!='GOVERSION']
 if mode=='omitted-empty' and a[-1].endswith('arch-metrics'):
  settings += [dict(Key=k) for k in ['CGO_CFLAGS','CGO_CPPFLAGS','CGO_CXXFLAGS','CGO_LDFLAGS']]
 if mode=='bad-setting' and a[-1].endswith('arch-metrics'):settings.append(dict(Key='CGO_CFLAGS',Value=7))
 print(json.dumps(dict(Path='github.com/zalkilabs/zalki/'+('scripts/arch-metrics' if a[-1].endswith('arch-metrics') else 'cmd/zalki'),GoVersion=profile['GOVERSION'],
  Settings=settings)));sys.exit(0)
if a[0]=='run':print(json.dumps([dict(package='github.com/zalkilabs/zalki/cmd/zalki',test='TestFixture')]));sys.exit(0)
if a[0]=='test':
 if mode=='test-failure':print('real failing fixture',file=sys.stderr);sys.exit(7)
 profilepath=Path(next(v.split('=',1)[1] for v in a if v.startswith('-coverprofile=')))
 if mode!='no-profile':profilepath.write_text('mode: set\nfake.go:1.1,1.2 1 1\n')
 for package in ['cmd/zalki','internal/leaf']:
  p='github.com/zalkilabs/zalki/'+package
  if mode=='missing-terminal' and package=='internal/leaf':continue
  for action in ['run','pass']:print(json.dumps(dict(Package=p,Test='TestFixture',Action=action)))
  print(json.dumps(dict(Package=p,Action='pass')))
 if mode=='source-change':(cwd/'internal/leaf/p.go').write_text('package changed\n')
 sys.exit(0)
sys.exit(19)
'''


class CaptureTest(unittest.TestCase):
    def fixture(self, root, mode):
        repo = root/'repo'
        for name in ['cmd/zalki', 'internal/leaf']:
            p = repo/name
            p.mkdir(parents=True)
            (p/'p.go').write_text('package fixture\n')
            (p/'p_test.go').write_text('package fixture\n')
        (repo/'go.mod').write_text('module github.com/zalkilabs/zalki\n')
        for args in [['init', '-q'], ['add', '.'], ['-c', 'user.name=Fixture', '-c',
                     'user.email=fixture@example.invalid', '-c', 'commit.gpgsign=false', 'commit', '-qm', 'fixture']]:
            subprocess.run(['git', '-C', str(repo), *args], check=True, capture_output=True)
        sdk = root/'sdk'
        (sdk/'bin').mkdir(parents=True)
        (sdk/'mode').write_text(mode)
        go = sdk/'bin/go'
        go.write_text(FAKE.replace('PYTHON', sys.executable))
        go.chmod(0o700)
        tools = root/'tools'
        tools.mkdir()
        tool = tools/'arch-metrics'
        text = 'Build: darwin/arm64, go1.26.11-fake, CGO_ENABLED=1.\n## Packages\n| Package | Files | Source lines | Test lines |\n| cmd/zalki | 1 | 1 | 1 | 0 | |\n| internal/leaf | 1 | 1 | 1 | 0 | |\n## Functions over 60 lines\nNone.\n## Internal imports\n'
        if mode == 'missing-human':
            text = '{}'
        tool.write_text('#!'+sys.executable+'\nprint('+repr(text)+')\n')
        tool.chmod(0o700)
        return repo, root/'output', tools, str(go)

    def test_complete_capture_uses_separate_profile_and_private_environment(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            args = self.fixture(root, 'good')
            with patch.dict(os.environ, {'CHECKPOINT_SECRET': 'must-not-enter-child'}):
                self.assertEqual(runner.execute(*args), 0)
            result = json.loads((args[1]/'result.json').read_text())
            self.assertTrue(result['passed'])
            self.assertTrue(result['clean_after'])
            self.assertEqual(result['test_events']['packages'], 2)
            calls = [json.loads(line) for line in (root/'sdk/calls.jsonl').read_text().splitlines()]
            self.assertFalse(any(c['secret'] for c in calls))
            tests = [c['args'] for c in calls if c['args'][0] == 'test']
            self.assertEqual(len(tests), 1)
            self.assertIn('-cover', tests[0])
            self.assertNotIn('-race', tests[0])
            self.assertEqual(tests[0][-1], './...')
            self.assertTrue((args[1]/'coverage.receipt.json').exists())
            self.assertTrue((args[1]/'cli/build.log.receipt.json').exists())

    def test_discovery_oracles_and_source_failures_are_not_green(self):
        for mode in ['partial', 'wrong-cli', 'missing-human', 'no-profile', 'missing-terminal', 'source-change', 'bad-setting']:
            with self.subTest(mode=mode), tempfile.TemporaryDirectory() as directory:
                args = self.fixture(Path(directory), mode)
                with self.assertRaises(ValueError):
                    runner.execute(*args)
                result = json.loads((args[1]/'result.json').read_text())
                self.assertFalse(result['passed'])

    def test_omitted_empty_sdk_setting_values_are_accepted(self):
        with tempfile.TemporaryDirectory() as directory:
            args = self.fixture(Path(directory), 'omitted-empty')
            self.assertEqual(runner.execute(*args), 0)
            result = json.loads((args[1]/'result.json').read_text())
            self.assertTrue(result['passed'])
            settings = result['architecture_tool_buildinfo']['Settings']
            self.assertEqual(sum('Value' not in v for v in settings), 4)

    def test_actual_test_failure_retains_both_streams_and_exit(self):
        with tempfile.TemporaryDirectory() as directory:
            args = self.fixture(Path(directory), 'test-failure')
            with self.assertRaises(ValueError):
                runner.execute(*args)
            receipt = json.loads((args[1]/'coverage.receipt.json').read_text())
            self.assertEqual(receipt['exit'], 7)
            self.assertFalse(receipt['timed_out'])
            self.assertIn('real failing fixture', (args[1]/'coverage.stderr').read_text())

    def test_owned_timeout_retains_real_outcome(self):
        with tempfile.TemporaryDirectory() as directory:
            folder = Path(directory)
            env = dict(os.environ, GOOS='darwin', GOARCH='arm64', CGO_ENABLED='1')
            with self.assertRaises(ValueError):
                runner.capture.command([sys.executable, '-c', 'import time;time.sleep(10)'],
                                       folder, env, folder, 'timed', timeout=0.05)
            receipt = json.loads((folder/'timed.receipt.json').read_text())
            self.assertTrue(receipt['timed_out'])
            self.assertLess(receipt['exit'], 0)


if __name__ == '__main__':
    unittest.main()
