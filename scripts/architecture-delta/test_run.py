"""Actual runner construction with synthetic Git/Go/snapshot executables only."""
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

HERE = Path(__file__).resolve().parent
BASE, HEAD = 'a'*40, 'b'*40


class ArchitectureOrchestration(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix='architecture source ')
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.bin = self.root/'toolchain/bin'; self.bin.mkdir(parents=True)
        self.tools = self.root/'tools'; self.tools.mkdir()
        self.strict = self.root/'strict'; self.strict.mkdir()
        self.output = self.root/'architecture'
        self.build = dict(GOOS='darwin',GOARCH='fixture-arch',CGO_ENABLED='1',GOVERSION='fixture-go')
        self.proof = dict(passed=True,base=BASE,head=HEAD,platform=self.build,trees={})
        paths = dict(self.build,GOROOT=str(self.bin.parent),GOCACHE=str(self.root/'go-cache'),
                     GOMODCACHE=str(self.root/'module-cache'),GOPATH=str(self.root/'go-path'))
        for key in ['GOCACHE','GOMODCACHE','GOPATH']:Path(paths[key]).mkdir()
        self.write(self.bin/'go', 'import json\nprint('+repr(json.dumps(paths))+')\n')
        git = "from pathlib import Path\nimport sys\na=sys.argv[1:];tree=Path(a[1]);a=a[2:]\nif a[0]=='rev-parse':print((tree/'.commit').read_text())\nelif a[0]=='status':print('')\nelse:raise SystemExit('unexpected Git command')\n"
        self.write(self.bin/'git',git)
        # The confined environment intentionally excludes the ambient bin.
        self.write(self.bin/'python3','')
        (self.bin/'python3').unlink();(self.bin/'python3').symlink_to(sys.executable)
        self.snapshot = dict(build=self.build,facts=dict(exports=['internal/p.Visible'],
            shapes={'function:internal/p.Visible':'func()'},receivers={},imports=[],os_calls={}),os_call_exceptions=[])
        for label,commit in [('base',BASE),('head',HEAD)]:
            tree=self.strict/'trees'/label;tree.mkdir(parents=True)
            (tree/'.commit').write_text(commit)
            (tree/'source.go').write_text('package p\nfunc Visible() {}\n')
            (tree/'snapshot.json').write_text(json.dumps(self.snapshot))
            self.proof['trees'][label]=dict(source_fingerprint=self.fingerprint(tree))
        self.save()
        self.approvals=self.root/'approved.json';self.approvals.write_text('[]')
        self.write(self.tools/'arch-metrics', '''from pathlib import Path
import json,os,sys
assert 'ZALKI_CAPABILITY' not in os.environ
assert os.environ['GOPROXY']=='off' and os.environ['GOENV']=='off'
assert os.environ['RUNNER_TRACKING_ID']=='synthetic-cleanup-tracking'
a=sys.argv[1:];assert a[0]=='-C' and a[2]=='-json';tree=Path(a[1])
if (tree/'tool-fails').exists():raise SystemExit('synthetic tool failure')
if (tree/'dirty').exists():(tree/'generated.go').write_text('unbound')
if (tree/'malformed').exists():print('not JSON')
else:print((tree/'snapshot.json').read_text())
''')

    def write(self,path,body):
        path.write_text('#!'+sys.executable+'\n'+body);path.chmod(0o700)

    def fingerprint(self,root):
        rows=[[p.relative_to(root).as_posix(),p.stat().st_mode&0o777,hashlib.sha256(p.read_bytes()).hexdigest()]
              for p in root.rglob('*') if p.is_file()]
        return dict(files=len(rows),sha256=hashlib.sha256(json.dumps(sorted(rows),separators=(',',':')).encode()).hexdigest())

    def save(self):
        (self.strict/'result.json').write_text(json.dumps(self.proof))

    def refresh(self,label):
        tree=self.strict/'trees'/label
        self.proof['trees'][label]['source_fingerprint']=self.fingerprint(tree);self.save()

    def invoke(self):
        env=dict(PATH=str(self.bin)+':/usr/bin:/bin',HOME=str(self.root),LANG='C',LC_ALL='C',
                 ZALKI_CAPABILITY='synthetic-do-not-forward',RUNNER_TRACKING_ID='synthetic-cleanup-tracking')
        return subprocess.run([sys.executable,str(HERE/'run.py'),'--lint-result',str(self.strict/'result.json'),
            '--tools',str(self.tools),'--output',str(self.output),'--approvals',str(self.approvals)],
            env=env,capture_output=True,text=True,timeout=20)

    def test_actual_construction_reuses_checked_trees_and_binds_binary_and_reports(self):
        result=self.invoke();self.assertEqual(result.returncode,0,result.stderr)
        proof=json.loads((self.output/'result.json').read_text())
        self.assertTrue(proof['passed']);self.assertEqual(proof['base'],BASE);self.assertEqual(proof['head'],HEAD)
        self.assertEqual(proof['snapshot_binary_sha256'],hashlib.sha256((self.tools/'arch-metrics').read_bytes()).hexdigest())
        self.assertEqual(proof['strict_result_sha256'],hashlib.sha256((self.strict/'result.json').read_bytes()).hexdigest())
        self.assertEqual(set(proof['commands']),{'base','head'})
        self.assertEqual(len(list((self.strict/'trees').iterdir())),2)
        self.assertFalse((HERE/'__pycache__').exists())

    def test_changed_literal_api_requires_exact_review_without_fixed_counts(self):
        self.snapshot['facts']['shapes']['function:internal/p.Visible']='func(string)'
        (self.strict/'trees/head/snapshot.json').write_text(json.dumps(self.snapshot));self.refresh('head')
        result=self.invoke();self.assertEqual(result.returncode,1,result.stderr)
        proof=json.loads((self.output/'result.json').read_text());self.assertFalse(proof['passed'])
        self.assertEqual(proof['result']['export_count_before'],proof['result']['export_count_after'])
        self.assertEqual(proof['result']['delta']['shapes']['changed']['function:internal/p.Visible'],dict(before='func()',after='func(string)'))
        self.approvals.write_text(json.dumps([dict(base_commit=BASE,os='darwin',delta=proof['result']['delta'],reason='Reviewed exact signature change')]))
        self.output=self.root/'approved-report'
        approved=self.invoke();self.assertEqual(approved.returncode,0,approved.stderr)
        self.assertEqual(json.loads((self.output/'result.json').read_text())['result']['approval_used'],'Reviewed exact signature change')

    def test_failed_strict_unbound_tree_and_wrong_commit_refuse(self):
        for case in ['strict-failure','source-change','wrong-commit']:
            with self.subTest(case=case):
                item=ArchitectureOrchestration();item.setUp()
                try:
                    if case=='strict-failure':item.proof['passed']=False;item.save()
                    elif case=='source-change':(item.strict/'trees/head/source.go').write_text('changed after strict')
                    else:(item.strict/'trees/head/.commit').write_text('c'*40);item.refresh('head')
                    result=item.invoke();self.assertEqual(result.returncode,2,result.stderr)
                    self.assertFalse((item.output/'result.json').exists())
                finally:item.doCleanups()

    def test_tool_failure_malformed_output_and_tool_source_side_effects_refuse(self):
        for marker in ['tool-fails','malformed','dirty']:
            with self.subTest(marker=marker):
                item=ArchitectureOrchestration();item.setUp()
                try:
                    (item.strict/'trees/head'/marker).write_text('yes');item.refresh('head')
                    result=item.invoke();self.assertEqual(result.returncode,2,result.stderr)
                    self.assertFalse((item.output/'result.json').exists())
                    self.assertFalse(json.loads((item.output/'failure.json').read_text())['passed'])
                finally:item.doCleanups()

    def test_snapshot_platform_must_match_the_actual_strict_build(self):
        self.snapshot['build']=dict(self.build,GOARCH='different')
        for label in ['base','head']:
            (self.strict/'trees'/label/'snapshot.json').write_text(json.dumps(self.snapshot));self.refresh(label)
        result=self.invoke();self.assertEqual(result.returncode,2,result.stderr)
        self.assertIn('snapshot build identity differs',result.stderr)

    def test_existing_output_cannot_be_relabelled_success_or_overwritten(self):
        self.output.mkdir();sentinel=self.output/'result.json';sentinel.write_text('historical')
        result=self.invoke();self.assertEqual(result.returncode,2,result.stderr)
        self.assertEqual(sentinel.read_text(),'historical');self.assertFalse((self.output/'failure.json').exists())


if __name__=='__main__':unittest.main()
