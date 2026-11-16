"""End-to-end construction with real make and fake Git/Go/linter/scoper tools."""
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
from unittest.mock import MagicMock, patch

HERE = Path(__file__).resolve().parent
SOURCE = HERE.parent.parent
spec = importlib.util.spec_from_file_location('lint_runner', HERE/'run.py')
lint_runner = importlib.util.module_from_spec(spec)
spec.loader.exec_module(lint_runner)
BASE, HEAD = 'a'*40, 'b'*40


class LintOrchestration(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix='lint delta ')
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.bin = self.root/'go-root/bin'; self.bin.mkdir(parents=True)
        self.tools = self.root/'tools'; self.tools.mkdir()
        self.log = self.root/'calls.jsonl'
        for name in ['go-cache','module-cache','go-path']:
            (self.root/name).mkdir()
        self.write(self.bin/'python3', '')
        (self.bin/'python3').unlink(); (self.bin/'python3').symlink_to(sys.executable)
        for label in ['base','head']:
            directory = self.root/label; directory.mkdir()
            shutil.copyfile(SOURCE/'Makefile',directory/'Makefile')
            (directory/'.golangci.yml').write_text('fixture only\n')
            (directory/'sample.go').write_text('package fixture\nfunc sample() {}\n')
            (directory/'scenario.json').write_text(json.dumps(dict(label=label,mode='good')))
        constants = 'ROOT=Path('+repr(str(self.root))+')\nCALLS=ROOT/"calls.jsonl"\n'
        common = 'from pathlib import Path\nimport sys,os,json,shutil\n'+constants
        git = common+'''
a=sys.argv[1:];cwd=Path(a[1]);a=a[2:]
if a[:2]==['worktree','add']:
 target=Path(a[-2]);commit=a[-1];label='base' if commit=='a'*40 else 'head'
 shutil.copytree(ROOT/label,target);(target/'.fixture-commit').write_text(commit)
 print('prepared fake worktree')
elif a[0]=='status':
 print('?? changed.go' if (cwd/'changed.go').exists() else '')
elif a[0]=='rev-parse':
 value=a[-1]
 if value=='HEAD': print((cwd/'.fixture-commit').read_text() if (cwd/'.fixture-commit').exists() else 'b'*40)
 elif value.endswith('^{tree}'): print('c'*40)
 elif value.endswith('^'): print('a'*40)
 else: print(value.removesuffix('^{commit}'))
else: raise SystemExit('unexpected fake git command')
'''
        go = common+'''
a=sys.argv[1:]
if a[0]=='env':
 print(json.dumps(dict(GOCACHE=str(ROOT/'go-cache'),GOMODCACHE=str(ROOT/'module-cache'),GOPATH=str(ROOT/'go-path'),GOROOT=str(ROOT/'go-root'),CGO_ENABLED='1',GOOS='fixture-os',GOARCH='fixture-arch',GOVERSION='fixture-go')))
elif a==['mod','download']:
 assert os.environ['GOPROXY']=='https://proxy.golang.org'
 with CALLS.open('a') as f:f.write(json.dumps(dict(kind='download',cwd=os.getcwd()))+'\\n')
else: raise SystemExit('REAL GO MUST NOT RUN')
'''
        linter = common+'''
a=sys.argv[1:];scenario=json.loads(Path('scenario.json').read_text());cache=Path(os.environ['GOLANGCI_LINT_CACHE'])
assert 'ZALKI_CAPABILITY' not in os.environ and os.environ['GOENV']=='off' and os.environ['GOFLAGS']=='-mod=readonly -p=2'
assert os.environ['GOPROXY']=='off' and os.environ['GOTOOLCHAIN']=='local'
assert Path(os.environ['CODEX_HOME']).parent==Path(os.environ['HOME'])
strict='--issues-exit-code' in a
with CALLS.open('a') as f:f.write(json.dumps(dict(kind='lint',strict=strict,cwd=os.getcwd(),cache=str(cache),fresh=not any(cache.iterdir()),args=a,home=os.environ['HOME'],tracking_present='RUNNER_TRACKING_ID' in os.environ))+'\\n')
(cache/'invoked').write_text('yes')
if scenario['mode']=='tool-error': raise SystemExit(3)
if not strict:
 (cache/'normal-ran').write_text('yes')
else:
 count=1
 if scenario['mode']=='addition':count=2
 if scenario['mode']=='order' and not (cache/'normal-ran').exists():count=2
 issue=dict(FromLinter='funlen',Text='literal diagnostic',Pos=dict(Filename='sample.go',Line=2))
 target=Path(a[a.index('--output.json.path')+1]);target.write_text(json.dumps(dict(Issues=[issue]*count)))
 if scenario['mode']=='duplicate-raw':target.write_text('{"Issues":'+json.dumps([issue])+',"Issues":[]}')
 if scenario['mode']=='dirty':Path('changed.go').write_text('package fixture\\n')
 if scenario['mode']=='ignored':Path('ignored.go').write_text('package fixture\\n')
'''
        scoper = common+'''
root,report=map(Path,sys.argv[1:]);scenario=json.loads((root/'scenario.json').read_text())
if scenario['mode']=='bad-scope': print('not JSON');raise SystemExit(0)
data=json.loads(report.read_text())
print(json.dumps([dict(linter=x['FromLinter'],file=x['Pos']['Filename'],scope='sample',text=x['Text'],line=x['Pos']['Line'],source_line=(root/'sample.go').read_text().splitlines()[x['Pos']['Line']-1].strip()) for x in data['Issues']]))
'''
        for path,body in [(self.bin/'git',git),(self.bin/'go',go),(self.tools/'golangci-lint',linter),
                          (self.tools/'issue-scopes',scoper),(self.tools/'gocognit',common+'pass\n')]:
            self.write(path,body)

    def write(self,path,body):
        path.write_text('#!'+sys.executable+'\n'+body);path.chmod(0o700)

    def invoke(self,mode='good',base=BASE,event='pull_request'):
        (self.root/'head/scenario.json').write_text(json.dumps(dict(label='head',mode=mode)))
        environment=dict(PATH=str(self.bin)+':/usr/bin:/bin',HOME=str(self.root),LANG='C',LC_ALL='C',
                         ZALKI_CAPABILITY='synthetic-do-not-forward',RUNNER_TRACKING_ID='synthetic-cleanup-tracking')
        command=[sys.executable,str(HERE/'run.py'),'--repo',str(self.root),'--base',base,'--head',HEAD,
                 '--event',event,'--tools',str(self.tools),'--output',str(self.root/'report')]
        result=subprocess.run(command,env=environment,stdout=subprocess.PIPE,stderr=subprocess.PIPE,text=True,timeout=30)
        return result

    def test_actual_construction_both_trees_both_orders_and_fresh_caches(self):
        result=self.invoke();self.assertEqual(result.returncode,0,result.stderr)
        proof=json.loads((self.root/'report/result.json').read_text())
        self.assertTrue(proof['passed']);self.assertEqual(proof['base'],BASE);self.assertEqual(proof['head'],HEAD)
        self.assertFalse((HERE/'__pycache__').exists())
        calls=[json.loads(x) for x in self.log.read_text().splitlines()]
        self.assertEqual(len([x for x in calls if x['kind']=='download']),2)
        lint=[x for x in calls if x['kind']=='lint'];self.assertEqual(len(lint),12)
        self.assertEqual(len({x['cache'] for x in lint}),4)
        self.assertEqual(sum(x['fresh'] for x in lint),4)
        self.assertTrue(all(x['tracking_present'] for x in lint))
        self.assertEqual([x['strict'] for x in lint],[False,False,True,True,False,False]*2)
        strict=[x for x in lint if x['strict']]
        self.assertTrue(all('--output.json.path' in x['args'] and '--output.text.path' in x['args'] for x in strict))
        self.assertTrue(all('--output.json.path' not in x['args'] for x in lint if not x['strict']))
        self.assertEqual(set(proof['orders']),{'base-normal-first','base-strict-first','head-normal-first','head-strict-first'})
        self.assertEqual(proof['tool_sha256']['golangci-lint'],hashlib.sha256((self.tools/'golangci-lint').read_bytes()).hexdigest())

    def test_raw_duplicate_diagnostic_addition_fails(self):
        result=self.invoke('addition');self.assertEqual(result.returncode,1,result.stderr)
        proof=json.loads((self.root/'report/result.json').read_text())
        self.assertFalse(proof['passed']);self.assertEqual(proof['result']['added'][0]['count'],1)

    def test_changed_source_only_is_a_regression_with_same_other_keys_and_count(self):
        (self.root/'head/sample.go').write_text('package fixture\nfunc sample() { /* changed */ }\n')
        result=self.invoke();self.assertEqual(result.returncode,1,result.stderr)
        proof=json.loads((self.root/'report/result.json').read_text())
        self.assertEqual(proof['result']['base_diagnostics'],proof['result']['head_diagnostics'])
        self.assertEqual(proof['result']['added'][0]['source_line'],'func sample() { /* changed */ }')

    def test_order_disagreement_fails_without_added_normal_key(self):
        result=self.invoke('order');self.assertEqual(result.returncode,1,result.stderr)
        proof=json.loads((self.root/'report/result.json').read_text())
        self.assertEqual(proof['result']['added'],[]);self.assertFalse(proof['result']['head_orders_equal'])

    def test_failed_tool_malformed_scopes_and_source_changes_are_not_success(self):
        for mode in ['tool-error','bad-scope','duplicate-raw','dirty','ignored']:
            with self.subTest(mode=mode):
                # A new test instance gives each failure an actually fresh output.
                fixture=LintOrchestration();fixture.setUp()
                try:
                    result=fixture.invoke(mode);self.assertNotEqual(result.returncode,0)
                    self.assertFalse((fixture.root/'report/result.json').exists())
                    self.assertFalse(json.loads((fixture.root/'report/failure.json').read_text())['passed'])
                finally:fixture.doCleanups()

    def test_invalid_or_missing_pr_base_refuses_before_worktree_construction(self):
        for value in ['', '0'*40, '--not-a-revision']:
            result=self.invoke(base=value);self.assertNotEqual(result.returncode,0)
            self.assertFalse((self.root/'report').exists())

    def test_existing_output_refused_without_changing_stale_evidence(self):
        directory=self.root/'report';directory.mkdir();sentinel=directory/'result.json';sentinel.write_text('stale')
        result=self.invoke();self.assertNotEqual(result.returncode,0)
        self.assertEqual(sentinel.read_text(),'stale');self.assertFalse((directory/'failure.json').exists())

    def test_command_timeout_signals_only_its_unreaped_session_then_joins(self):
        child=MagicMock();child.pid=12345
        child.wait.side_effect=[subprocess.TimeoutExpired('fixture',0.1),-9]
        with patch.object(lint_runner.subprocess,'Popen',return_value=child) as start, patch.object(lint_runner.os,'killpg') as kill:
            with self.assertRaises(subprocess.TimeoutExpired):
                lint_runner.run_command(['fixture'],self.root,{},self.root/'timeout.log',timeout=0.1)
            self.assertTrue(start.call_args.kwargs['start_new_session'])
            kill.assert_called_once_with(12345,lint_runner.signal.SIGKILL)
            self.assertEqual(child.wait.call_args_list[0].kwargs,{'timeout':0.1})
            self.assertEqual(child.wait.call_args_list[1].kwargs,{'timeout':10})

    def test_joined_command_never_signals_recorded_process_identity(self):
        child=MagicMock();child.wait.return_value=0
        with patch.object(lint_runner.subprocess,'Popen',return_value=child), patch.object(lint_runner.os,'killpg') as kill:
            self.assertEqual(lint_runner.run_command(['fixture'],self.root,{},self.root/'joined.log')['exit'],0)
            kill.assert_not_called();child.wait.assert_called_once()

    def test_unbound_source_file_and_directory_links_refuse_before_lint(self):
        for directory_link in [False,True]:
            with self.subTest(directory_link=directory_link):
                fixture=LintOrchestration();fixture.setUp()
                try:
                    outside=fixture.root/'outside';outside.mkdir()
                    external=outside/'external.go';external.write_text('package external\nfunc Unbound() {}\n')
                    link=fixture.root/'head'/('linked-dir' if directory_link else 'linked.go')
                    link.symlink_to(outside if directory_link else external,target_is_directory=directory_link)
                    # Preserve links in the fake Git clone as real Git does.
                    fake=fixture.bin/'git';fake.write_text(fake.read_text().replace('shutil.copytree(ROOT/label,target)','shutil.copytree(ROOT/label,target,symlinks=True)'))
                    result=fixture.invoke();self.assertNotEqual(result.returncode,0)
                    self.assertIn('source symlinks are not supported',result.stderr)
                    self.assertFalse((fixture.root/'report/result.json').exists())
                    calls=[json.loads(x) for x in fixture.log.read_text().splitlines()]
                    self.assertFalse(any(x['kind']=='lint' and x['cwd'].endswith('/head') for x in calls))
                finally:fixture.doCleanups()

    def test_tag_or_manual_first_parent_is_explicit(self):
        result=self.invoke(base='',event='tag');self.assertEqual(result.returncode,0,result.stderr)
        self.assertEqual(json.loads((self.root/'report/result.json').read_text())['base'],BASE)


if __name__=='__main__':unittest.main()
