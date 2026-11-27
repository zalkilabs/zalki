"""Closed profiles/native accounting and real phase construction with fake tools."""
import copy
import json
import os
from pathlib import Path
import subprocess
import unittest
import test_run as fixtures
import execution
import run


class Profiles(unittest.TestCase):
    def control(self):
        item=fixtures.FocusedConstruction();item.setUp();self.addCleanup(item.doCleanups)
        return item,copy.deepcopy(item.control)

    def test_existing_profile_stays_raced_and_bounded(self):
        item,control=self.control();self.assertEqual(execution.profile(control),dict(race=True,test_seconds=45,process_seconds=120,build_seconds=300,cli=False,functional_sites=False))
        self.assertTrue(execution.applicable(control,'linux'));self.assertTrue(execution.applicable(control,'darwin'))
        control['profile']='r7_background';self.assertEqual(execution.profile(control),dict(race=False,test_seconds=120,process_seconds=150,build_seconds=240,cli=True,functional_sites=True))

    def test_unreviewed_profiles_or_native_masks_refuse(self):
        item,base=self.control()
        changes=[dict(profile='arbitrary'),dict(profile={'test_seconds':99999}),dict(native_os=[]),dict(native_os=['windows']),
                 dict(native_os=['darwin','darwin']),dict(rebuild_cli_for_mutant=True),dict(rebuild_cli_for_mutant='true'),dict(extra_env={'TOKEN':'ignored'})]
        for change in changes:
            with self.subTest(change=change),self.assertRaises(ValueError):run.validate_controls([dict(base,**change)])
        for kind in ['const','var']:
            candidate=copy.deepcopy(base);candidate['mutation']['declaration_kind']=kind
            run.validate_controls([candidate])
        candidate=copy.deepcopy(base);candidate['mutation']['declaration_kind']='arbitrary'
        with self.assertRaises(ValueError):run.validate_controls([candidate])

    def test_darwin_only_is_accounted_without_a_linux_execution(self):
        item,control=self.control();control['native_os']=['darwin'];item.plan.write_text(json.dumps([control]))
        result=item.invoke();self.assertEqual(result.returncode,0,result.stderr)
        report=json.loads((item.output/'result.json').read_text());self.assertEqual(report['executed_controls'],0,'DARWIN_ONLY_CONTROL_MUST_NOT_EXECUTE_ON_LINUX')
        self.assertEqual(report['not_applicable_controls'],[dict(id=control['id'],native_os=['darwin'],actual_os='linux',reason='declared_native_applicability')])
        calls=[json.loads(line)['argv'] for line in (item.root/'calls.jsonl').read_text().splitlines()]
        self.assertFalse(any(a[0]=='test' for a in calls));self.assertFalse((item.output/control['id']).exists())
        self.assertTrue(execution.applicable(control,'darwin'))



def configure_cli_tools(item):
    git=item.bin/'git';text=git.read_text()
    text=text.replace("elif a[0]=='rev-parse':", "elif a[0]=='clone':\n assert a[1:4]==['--no-hardlinks','--no-checkout','--']\n target=Path(a[-1]);shutil.copytree(ROOT/'repo',target);(target/'.git').mkdir()\nelif a[0]=='checkout':assert a==['checkout','--detach','b'*40]\nelif a[0]=='rev-parse':")
    text=text.replace("elif a[0]=='status':print('')", "elif a[0]=='status':\n current=cwd/'internal/fixture/guard.go'\n print(' M internal/fixture/guard.go' if current.read_bytes()!=(ROOT/'repo/internal/fixture/guard.go').read_bytes() else '')")
    git.write_text(text)
    go=item.bin/'go';text=go.read_text()
    branches="""elif a[0]=='build':
 assert '-trimpath' in a and '-race=false' in a and '-buildvcs=true' in a
 assert Path('.git').is_dir(),'R7_CLI_REAL_GIT_REQUIRED'
 Path(a[a.index('-o')+1]).write_bytes(Path('internal/fixture/guard.go').read_bytes())
elif a[0]=='version':
 bad=b'permitSynthetic() bool { return true }' in Path(a[-1]).read_bytes()
 settings=dict(GOOS=PATHS['GOOS'],GOARCH=PATHS['GOARCH'],CGO_ENABLED=PATHS['CGO_ENABLED'],**{'vcs.revision':'b'*40,'vcs.modified':str(bad).lower()})
 print(json.dumps(dict(Path='github.com/zalkilabs/zalki/cmd/zalki',GoVersion=PATHS['GOVERSION'],Settings=[dict(Key=k,Value=v) for k,v in settings.items()]+[dict(Key='CGO_CFLAGS')])))
"""
    text=text.replace("elif a[0]=='test':",branches+"elif a[0]=='test':")
    text=text.replace("if (ROOT/'compile-error').exists():raise SystemExit(2)", "assert '-trimpath' in a and '-race=false' in a\n if (ROOT/'compile-error').exists():raise SystemExit(2)")
    text=text.replace("binary=Path(a[a.index('-p')+2]);bad=b'permitSynthetic() bool { return true }' in binary.read_bytes()", "binary=Path(a[a.index('-p')+2]);bad=b'permitSynthetic() bool { return true }' in Path(os.environ['ZALKI_TEST_BINARY']).read_bytes()\n assert '-test.timeout=120s' in a and '-test.count=1' in a")
    go.write_text(text)


class RealCLIConstruction(unittest.TestCase):
    def test_profile_rebuilds_the_real_mutant_CLI_and_reads_back_restoration(self):
        item=fixtures.FocusedConstruction();item.setUp();self.addCleanup(item.doCleanups);configure_cli_tools(item)
        item.control.update(profile='r7_background',rebuild_cli_for_mutant=True);item.plan.write_text(json.dumps([item.control]))
        result=item.invoke();self.assertEqual(result.returncode,0,'REAL_MUTANT_CLI_REBUILD_REQUIRED: '+result.stderr)
        proof=json.loads((item.output/item.control['id']/'result.json').read_text())
        hashes=proof['result']['phase_CLI_sha256']
        self.assertNotEqual(hashes['good'],hashes['intended'],'REAL_MUTANT_CLI_REBUILD_REQUIRED')
        self.assertEqual(hashes['good'],hashes['restored'])
        self.assertFalse(proof['context']['phases']['good']['cli']['modified'])
        self.assertTrue(proof['context']['phases']['intended']['cli']['modified'])
        self.assertNotEqual(proof['context']['phases']['good']['cli']['source_fingerprint'],proof['context']['phases']['intended']['cli']['source_fingerprint'])
        self.assertEqual(proof['context']['phases']['good']['cli']['settings']['vcs.revision'],fixtures.HEAD)
        calls=[json.loads(line) for line in (item.root/'calls.jsonl').read_text().splitlines()]
        builds=[c for c in calls if c['argv'][0]=='build'];self.assertEqual(len(builds),2)
        self.assertEqual(len({c['home'] for c in builds}),2)
        self.assertEqual((item.output/item.control['id']/'source/internal/fixture/guard.go').read_bytes(),(item.repo/'internal/fixture/guard.go').read_bytes())
