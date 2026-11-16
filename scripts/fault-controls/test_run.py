"""Actual focused-runner construction using fake Git/Go/AST executables only."""
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import MagicMock,patch

HERE=Path(__file__).resolve().parent
STRICT=HERE.parent/'lint-delta'
BASE,HEAD='a'*40,'b'*40


class FocusedConstruction(unittest.TestCase):
    def setUp(self):
        temporary=tempfile.TemporaryDirectory(prefix='focused control ');self.addCleanup(temporary.cleanup)
        self.root=Path(temporary.name);self.bin=self.root/'go-root/bin';self.bin.mkdir(parents=True)
        self.repo=self.root/'repo';self.repo.mkdir();self.tools=self.root/'tools';self.tools.mkdir()
        self.output=self.root/'report';self.layout=self.root/'layout/scripts/fault-controls'
        shutil.copytree(HERE,self.layout,ignore=shutil.ignore_patterns('__pycache__'))
        shutil.copytree(STRICT,self.layout.parent/'lint-delta')
        synthetic=json.loads((HERE/'fixtures/direct-named-assertion-positive.json').read_text())
        package=self.repo/'internal/fixture';package.mkdir(parents=True)
        original=synthetic['context']['phases']['good']['source_files']
        (package/'guard.go').write_text(original['guard_fixture.go'])
        (package/'guard_test.go').write_text(original['oracle_fixture_test.go'])
        (self.repo/'.fixture-head').write_text(HEAD)
        self.control=dict(id='SYNTHETIC-F1',kind='production',mutation=dict(path='internal/fixture/guard.go',symbol='permitSynthetic',
            old='return false',new='return true',required_occurrences=1),selection=dict(package='github.com/zalkilabs/zalki/internal/fixture',
            directory='./internal/fixture',test='TestSentinel/blue'),assertion=dict(synthetic['allowed_source_sites_and_caller_mapping'],
            file='internal/fixture/guard_test.go'),output_fragment='SYNTHETIC_GUARD_FAILED')
        self.plan=self.layout/'controls.json';self.plan.write_text(json.dumps([self.control]))
        paths=dict(GOROOT=str(self.bin.parent),GOCACHE=str(self.root/'go-cache'),GOMODCACHE=str(self.root/'module-cache'),
                   GOPATH=str(self.root/'go-path'),GOOS='linux',GOARCH='fixture-arch',GOVERSION='fixture-go',CGO_ENABLED='1')
        for k in ['GOCACHE','GOMODCACHE','GOPATH']:Path(paths[k]).mkdir()
        common='from pathlib import Path\nimport json,sys,os,shutil,hashlib\nROOT=Path('+repr(str(self.root))+')\n'
        self.write(self.bin/'git',common+'''
a=sys.argv[1:];cwd=Path(a[1]);a=a[2:]
if a[:2]==['worktree','add']:shutil.copytree(ROOT/'repo',Path(a[-2]))
elif a[0]=='rev-parse':print((cwd/'.fixture-head').read_text() if a[-1]=='HEAD' else a[-1].removesuffix('^{commit}'))
elif a[0]=='diff':print('.github/CONTRIBUTING.md' if (ROOT/'docs-only').exists() else 'internal/fixture/guard.go')
elif a[0]=='ls-tree':print('scripts/fault-controls/controls.json' if (ROOT/'baseline-plan.json').exists() else '')
elif a[0]=='show':print((ROOT/'baseline-plan.json').read_text())
elif a[0]=='status':print('')
else:raise SystemExit('unexpected fake Git command')
''')
        streams={label:synthetic['context']['phases'][label]['events'] for label in ['good','intended']}
        for events in streams.values():
            for event in events:
                event['Package']=self.control['selection']['package']
                if 'Output' in event:event['Output']=event['Output'].replace('oracle_fixture_test.go','guard_test.go')
        go=common+'PATHS='+repr(paths)+'\nSTREAMS='+repr(streams)+'''\na=sys.argv[1:]
with (ROOT/'calls.jsonl').open('a') as f:f.write(json.dumps(dict(argv=a,home=os.environ.get('HOME')))+'\\n')
if a[0]=='env':print(json.dumps(PATHS))
elif a==['mod','download']:assert os.environ['GOPROXY']=='https://proxy.golang.org'
elif a[0]=='test':
 assert os.environ['GOPROXY']=='off' and 'ZALKI_CAPABILITY' not in os.environ
 assert Path(os.environ['TMPDIR']).is_dir()
 if (ROOT/'compile-error').exists():raise SystemExit(2)
 Path(a[a.index('-o')+1]).write_bytes(Path('internal/fixture/guard.go').read_bytes())
elif a[:2]==['tool','test2json']:
 binary=Path(a[a.index('-p')+2]);bad=b'permitSynthetic() bool { return true }' in binary.read_bytes()
 for event in STREAMS['intended' if bad else 'good']:print(json.dumps(event))
 raise SystemExit(1 if bad else 0)
else:raise SystemExit('REAL GO MUST NOT RUN')
'''
        self.write(self.bin/'go',go)
        self.write(self.tools/'assertion-sites',common+'''
a=sys.argv[1:];tree=Path(a[1]);r=json.load(sys.stdin);raw=(tree/r['file']).read_bytes();m=r['mutation'];before=(tree/m['path']).read_bytes()
print(json.dumps(dict(file=r['file'],source_sha256=hashlib.sha256(raw).hexdigest(),function=r['assertion_function'],literal=r['fatal_literal'],
 reviewed_call=r['fatal_call'],assertion_sites=1,file_sites=1,helper_chain_valid=True,report_lines=[999 if (ROOT/'wrong-site').exists() else 7],
 mutation=dict(file=m['path'],symbol=m['symbol'],source_sha256=hashlib.sha256(before).hexdigest(),offset=before.index(m['old'].encode())))))
''')

    def write(self,path,body):path.write_text('#!'+sys.executable+'\n'+body);path.chmod(0o700)

    def invoke(self):
        env=dict(PATH=str(self.bin)+':/usr/bin:/bin',HOME=str(self.root),PYTHONDONTWRITEBYTECODE='1',
                 RUNNER_TRACKING_ID='synthetic-cleanup-tracking',ZALKI_CAPABILITY='synthetic-do-not-forward')
        return subprocess.run([sys.executable,str(self.layout/'run.py'),'--repo',str(self.repo),'--base',BASE,'--head',HEAD,
            '--event','pull_request','--tools',str(self.tools),'--output',str(self.output)],env=env,capture_output=True,text=True,timeout=30)

    def test_actual_good_intended_restored_construction_and_exact_source_restore(self):
        result=self.invoke();self.assertEqual(result.returncode,0,result.stderr)
        overall=json.loads((self.output/'result.json').read_text());self.assertTrue(overall['passed']);self.assertEqual(overall['executed_controls'],1)
        proof=json.loads((self.output/'SYNTHETIC-F1/result.json').read_text());phases=proof['context']['phases']
        self.assertEqual(phases['good']['source_files'],phases['restored']['source_files'])
        self.assertNotEqual(phases['good']['build']['binary_sha256'],phases['intended']['build']['binary_sha256'])
        self.assertEqual(phases['good']['build']['binary_sha256'],phases['restored']['build']['binary_sha256'])
        self.assertEqual(proof['result']['matched'][0]['test'],'TestSentinel/blue')
        actual=(self.output/'SYNTHETIC-F1/source/internal/fixture/guard.go').read_bytes()
        self.assertEqual(actual,(self.repo/'internal/fixture/guard.go').read_bytes())
        calls=[json.loads(line) for line in (self.root/'calls.jsonl').read_text().splitlines()]
        builds=[call for call in calls if call['argv'][0]=='test']
        self.assertEqual(len(builds),3);self.assertEqual(len({call['home'] for call in builds}),3)

    def test_compile_failure_and_wrong_source_site_are_not_killed_controls(self):
        for marker in ['compile-error','wrong-site']:
            with self.subTest(marker=marker):
                item=FocusedConstruction();item.setUp()
                try:
                    (item.root/marker).write_text('yes');result=item.invoke();self.assertEqual(result.returncode,2,result.stderr)
                    self.assertFalse((item.output/'result.json').exists())
                    self.assertFalse((item.output/'SYNTHETIC-F1/result.json').exists())
                    self.assertFalse(json.loads((item.output/'failure.json').read_text())['passed'])
                finally:item.doCleanups()

    def test_existing_success_cannot_be_overwritten_or_relabelled(self):
        self.output.mkdir();p=self.output/'result.json';p.write_text('historical')
        result=self.invoke();self.assertEqual(result.returncode,2,result.stderr)
        self.assertEqual(p.read_text(),'historical');self.assertFalse((self.output/'failure.json').exists())

    def test_escaping_plan_fails_before_go_or_worktree_creation(self):
        self.control['mutation']['path']='internal/../outside.go';self.plan.write_text(json.dumps([self.control]))
        result=self.invoke();self.assertEqual(result.returncode,2,result.stderr)
        self.assertFalse((self.root/'calls.jsonl').exists());self.assertFalse((self.output/'SYNTHETIC-F1').exists())

    def test_inherited_controls_cannot_disappear_from_the_head_plan(self):
        other=json.loads(json.dumps(self.control));other['id']='SYNTHETIC-F2'
        (self.root/'baseline-plan.json').write_text(json.dumps([self.control,other]))
        result=self.invoke();self.assertEqual(result.returncode,2,result.stderr)
        self.assertIn('baseline required controls were removed',result.stderr)
        self.assertFalse((self.root/'calls.jsonl').exists())

    def test_docs_only_delta_records_zero_execution_without_optional_job_skip(self):
        (self.root/'docs-only').write_text('yes')
        result=self.invoke();self.assertEqual(result.returncode,0,result.stderr)
        proof=json.loads((self.output/'result.json').read_text())
        self.assertFalse(proof['affected']);self.assertEqual(proof['executed_controls'],0)
        self.assertFalse((self.root/'calls.jsonl').exists())

    def test_timeout_stops_only_the_unreaped_owned_session_and_joins(self):
        spec=importlib.util.spec_from_file_location('focused_runner',self.layout/'run.py')
        module=importlib.util.module_from_spec(spec);spec.loader.exec_module(module)
        child=MagicMock();child.pid=12345;child.wait.side_effect=[subprocess.TimeoutExpired('fixture',0.1),-9]
        with patch.object(module.subprocess,'Popen',return_value=child),patch.object(module.os,'killpg') as signal:
            with self.assertRaises(subprocess.TimeoutExpired):module.command(['fixture'],self.repo,{},self.root/'timeout.log',0,timeout=0.1)
            signal.assert_called_once_with(12345,module.signal.SIGKILL)
            self.assertEqual(child.wait.call_args_list[1].kwargs,dict(timeout=10))
        child=MagicMock();child.wait.return_value=0
        with patch.object(module.subprocess,'Popen',return_value=child),patch.object(module.os,'killpg') as signal:
            module.command(['fixture'],self.repo,{},self.root/'joined.log',0)
            signal.assert_not_called()


if __name__=='__main__':unittest.main()
