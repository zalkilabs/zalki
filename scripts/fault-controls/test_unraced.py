"""Unraced selection/projection and actual fake-tool runner construction."""
import hashlib
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
import test_run as fixtures
import unraced

PREFIX=unraced.PREFIX


class UnracedSelection(unittest.TestCase):
    def setUp(self):
        temporary=tempfile.TemporaryDirectory();self.addCleanup(temporary.cleanup);self.root=Path(temporary.name)
        self.package=dict(Dir=str(self.root/'internal/one'),ImportPath=PREFIX+'/internal/one',TestGoFiles=['one_test.go'])
        self.second=dict(Dir=str(self.root/'internal/two'),ImportPath=PREFIX+'/internal/two',TestGoFiles=['two_test.go'])

    def test_direct_touched_packages_and_module_change_scope(self):
        selected,omitted=unraced.affected(self.root,['internal/one/file_test.go'],[self.package,self.second],[],lambda _:self.fail('unexpected probe'))
        self.assertEqual(selected,[self.package]);self.assertEqual(omitted,[])
        selected,_=unraced.affected(self.root,['go.mod'],[self.package,self.second],[],lambda _:self.fail('unexpected probe'))
        self.assertEqual(len(selected),2)

    def test_deleted_and_native_constraints_are_explicit_not_silent_omissions(self):
        directory=self.root/'internal/foreign';directory.mkdir(parents=True);(directory/'foreign_linux.go').write_text('package foreign')
        foreign=dict(Dir=str(directory),ImportPath=PREFIX+'/internal/foreign',IgnoredGoFiles=['foreign_linux.go'],
                     Error=dict(Err='build constraints exclude all Go files in '+str(directory)))
        selected,omitted=unraced.affected(self.root,['internal/foreign/foreign_linux.go','internal/deleted/a.go'],[self.package],[],lambda _:[foreign])
        self.assertEqual(selected,[]);self.assertEqual({r['reason'] for r in omitted},{'native_build_constraints','no_head_go_files'})
        foreign['Error']['Err']='unknown discovery failure'
        with self.assertRaisesRegex(ValueError,'discovery failed'):unraced.affected(self.root,['internal/foreign/foreign_linux.go'],[self.package],[],lambda _:[foreign])

    def test_partial_duplicate_empty_and_escaped_discovery_refuse(self):
        with self.assertRaises(ValueError):unraced.json_values('')
        with self.assertRaises(ValueError):unraced.json_values('{"Dir":"a","Dir":"b"}')
        with self.assertRaises(ValueError):unraced.affected(self.root,['internal/one/a.go'],[self.package,self.package],[],lambda _:[])
        wrong=dict(self.package,Dir='/outside')
        with self.assertRaises(ValueError):unraced.classify(wrong,self.root)
        with self.assertRaises(ValueError):unraced.classify(dict(self.package,Incomplete=True),self.root)

    def test_current_required_projection_refuses_missing_and_skipped_descendants(self):
        required=[dict(package=self.package['ImportPath'],test='TestNeeded'),dict(package=self.second['ImportPath'],test='TestOther')]
        package=self.package['ImportPath']
        events=[dict(Action=a,Package=package,Test='TestNeeded') for a in ['run','pass']]+[dict(Action='pass',Package=package)]
        raw=lambda rows:'\n'.join(map(json.dumps,rows))
        self.assertEqual(unraced.coverage(raw(events),[self.package],required)['required_roots'],1)
        with self.assertRaisesRegex(ValueError,'missing RUN/PASS'):unraced.coverage(raw(events[1:]),[self.package],required)
        hidden=dict(Action='skip',Package=package,Test='TestNeeded/hidden')
        with self.assertRaisesRegex(ValueError,'descendant skipped'):unraced.coverage(raw([hidden]+events),[self.package],required)
        with self.assertRaises(ValueError):unraced.coverage(raw(events),[self.package],[])

    def test_no_test_files_package_skip_requires_actual_discovery_and_report(self):
        package=dict(self.package,TestGoFiles=[])
        required=[dict(package=self.second['ImportPath'],test='TestOther')]
        events=[dict(Action='skip',Package=package['ImportPath'])]
        raw=lambda rows:'\n'.join(map(json.dumps,rows))
        with self.assertRaises(ValueError):unraced.coverage(raw(events),[package],required)
        events.insert(0,dict(Action='output',Package=package['ImportPath'],Output='? '+package['ImportPath']+' [no test files]\n'))
        self.assertEqual(unraced.coverage(raw(events),[package],required)['packages'],1)

    def test_fdprobe_c_only_and_nested_go_testdata_select_the_native_owner(self):
        cmd=dict(Dir=str(self.root/'cmd/zalki'),ImportPath=PREFIX+'/cmd/zalki',TestGoFiles=['fdprobe_test.go'])
        for name in ['cmd/zalki/testdata/fdprobe.c','cmd/zalki/testdata/broken/main.go']:
            with self.subTest(input=name):
                bindings=[]
                selected,omitted=unraced.affected(self.root,[name],[cmd,self.package],[],lambda _:self.fail('fixture treated as a package'),bindings=bindings)
                self.assertEqual([p['ImportPath'] for p in selected],[PREFIX+'/cmd/zalki']);self.assertEqual(omitted,[])
                self.assertEqual(bindings[0]['owners'],['cmd/zalki'])
                self.assertTrue(unraced.controls.relevant([name]))

    def test_non_go_fixture_deletion_retains_its_current_package_owner(self):
        selected,omitted=unraced.affected(self.root,['internal/one/testdata/removed.json'],[self.package],[],lambda _:self.fail('unexpected probe'))
        self.assertEqual(selected,[self.package]);self.assertEqual(omitted,[])

    def test_explicit_cross_package_inputs_and_runtime_recipe_mapping(self):
        policy=dict(paths={'internal/one/testdata/shared.json':['internal/two']},prefixes={},non_runtime_paths=[],non_runtime_prefixes=[])
        selected,_=unraced.affected(self.root,['internal/one/testdata/shared.json'],[self.package,self.second],[],lambda _:[],policy)
        self.assertEqual({p['ImportPath'] for p in selected},{self.package['ImportPath'],self.second['ImportPath']})
        plan=[dict(mutation=dict(path='.goreleaser.yaml'),selection=dict(directory='./internal/two'))]
        selected,_=unraced.affected(self.root,['.goreleaser.yaml'],[self.package,self.second],plan,lambda _:[])
        self.assertEqual(selected,[self.second])

    def test_unknown_runtime_input_and_missing_explicit_owner_refuse(self):
        with self.assertRaisesRegex(ValueError,'no reviewed/current owning package'):
            unraced.affected(self.root,['unowned/fdprobe.c'],[self.package],[],lambda _:[])
        policy=dict(paths={'runtime.json':['internal/missing']},prefixes={},non_runtime_paths=[],non_runtime_prefixes=[])
        with self.assertRaisesRegex(ValueError,'owner is missing'):
            unraced.affected(self.root,['runtime.json'],[self.package],[],lambda _:[],policy)

    def test_cli_metadata_requires_nonrace_and_exact_clean_head(self):
        paths=dict(GOVERSION='fixture-go',GOOS='linux',GOARCH='fixture',CGO_ENABLED='1')
        values=dict(GOOS='linux',GOARCH='fixture',CGO_ENABLED='1',**{'vcs.revision':'b'*40,'vcs.modified':'false'})
        info=dict(Path=PREFIX+'/cmd/zalki',GoVersion='fixture-go',Settings=[dict(Key=k,Value=v) for k,v in values.items()])
        self.assertEqual(unraced.cli_identity(info,'b'*40,paths)['vcs.modified'],'false')
        info['Settings'].append(dict(Key='-race',Value='true'))
        with self.assertRaisesRegex(ValueError,'race instrumentation'):unraced.cli_identity(info,'b'*40,paths)
        info['Settings'][-1]=dict(Key='-race',Value='false');info['Settings'][3]['Value']='a'*40
        with self.assertRaises(ValueError):unraced.cli_identity(info,'b'*40,paths)


    def test_hosted_cli_metadata_empty_values_keep_missing_vcs_a_closed_error(self):
        for native in ['linux','macos']:
            info=json.loads((Path(__file__).resolve().parent/'fixtures'/('cli-buildinfo-'+native+'-empty-settings.json')).read_text())
            settings={s['Key']:s.get('Value','') for s in info['Settings']}
            paths=dict(GOVERSION=info['GoVersion'],**{k:settings[k] for k in ['GOOS','GOARCH','CGO_ENABLED']})
            with self.subTest(native=native):
                with self.assertRaisesRegex(ValueError,'VCS identity differs'):unraced.cli_identity(info,'b'*40,paths)
                # Synthetic stamped metadata demonstrates normalization only;
                # it is not a claim that the original hosted CLI was stamped.
                info['Settings'] += [dict(Key='vcs.revision',Value='b'*40),dict(Key='vcs.modified',Value='false')]
                self.assertEqual(unraced.cli_identity(info,'b'*40,paths)['CGO_CFLAGS'],'', 'EMPTY_BUILD_SETTING_MUST_DECODE_AS_EMPTY')

    def test_cli_empty_setting_value_is_optional_but_identity_is_required(self):
        paths=dict(GOVERSION='fixture-go',GOOS='linux',GOARCH='fixture',CGO_ENABLED='1')
        values=dict(GOOS='linux',GOARCH='fixture',CGO_ENABLED='1',**{'vcs.revision':'b'*40,'vcs.modified':'false'})
        def info():
            return dict(Path=PREFIX+'/cmd/zalki',GoVersion='fixture-go',Settings=[dict(Key=k,Value=v) for k,v in values.items()]+[dict(Key='CGO_CFLAGS')])
        try:normalized=unraced.cli_identity(info(),'b'*40,paths)
        except KeyError:self.fail('EMPTY_BUILD_SETTING_MUST_DECODE_AS_EMPTY')
        self.assertEqual(normalized['CGO_CFLAGS'],'', 'EMPTY_BUILD_SETTING_MUST_DECODE_AS_EMPTY')
        for setting in [dict(Key='CGO_CFLAGS',Value=None),dict(Key=1),dict(Value='x'),dict(Key=''),dict(Key='GOOS',Value='linux')]:
            candidate=info();candidate['Settings'].append(setting)
            with self.subTest(setting=setting),self.assertRaises(ValueError):unraced.cli_identity(candidate,'b'*40,paths)
        for key in ['GOOS','GOARCH','CGO_ENABLED','vcs.revision','vcs.modified']:
            candidate=info();candidate['Settings']=[s for s in candidate['Settings'] if s['Key']!=key]
            with self.subTest(missing=key),self.assertRaises(ValueError):unraced.cli_identity(candidate,'b'*40,paths)


class UnracedConstruction(unittest.TestCase):
    def test_actual_fresh_cli_and_full_selected_package_argv_with_fake_tools(self):
        item=fixtures.FocusedConstruction();item.setUp();self.addCleanup(item.doCleanups)
        git=item.bin/'git';git_text=git.read_text()
        git_text=git_text.replace("elif a[0]=='rev-parse':", "elif a[0]=='clone':\n assert a[1:4]==['--no-hardlinks','--no-checkout','--']\n target=Path(a[-1]);shutil.copytree(ROOT/'repo',target);(target/'.git').mkdir()\nelif a[0]=='checkout':assert a==['checkout','--detach','b'*40]\nelif a[0]=='rev-parse':")
        git.write_text(git_text)
        script=item.bin/'go';text=script.read_text()
        addition='''elif a[0]=='list':
 print(json.dumps(dict(Dir=str(Path.cwd()/'internal/fixture'),ImportPath='github.com/zalkilabs/zalki/internal/fixture',TestGoFiles=['guard_test.go'])))
elif a[0]=='build':
 assert '-race=false' in a and '-buildvcs=true' in a
 assert Path('.git').is_dir(),'FRESH_CLI_REAL_GIT_DIRECTORY_REQUIRED'
 Path(a[a.index('-o')+1]).write_bytes(b'fresh nonrace CLI')
elif a[0]=='version':
 settings=dict(GOOS='linux',GOARCH='fixture-arch',CGO_ENABLED='1',**{'vcs.revision':'b'*40,'vcs.modified':'false'})
 print(json.dumps(dict(Path='github.com/zalkilabs/zalki/cmd/zalki',GoVersion='fixture-go',Settings=[dict(Key=k,Value=v) for k,v in settings.items()]+[dict(Key='CGO_CFLAGS')])))
elif a[0]=='test' and '-c' not in a:
 assert '-race=false' in a and not any(x.startswith('-run') for x in a)
 assert Path(os.environ['ZALKI_TEST_BINARY']).read_bytes()==b'fresh nonrace CLI'
 for event in STREAMS['good']:print(json.dumps(event))
'''
        text=text.replace("elif a[0]=='test':",addition+"elif a[0]=='test':");script.write_text(text)
        item.write(item.tools/'required-coverage',"import json\nprint(json.dumps([dict(package='github.com/zalkilabs/zalki/internal/fixture',test='TestSentinel')]))\n")
        env=dict(PATH=str(item.bin)+':/usr/bin:/bin',HOME=str(item.root),PYTHONDONTWRITEBYTECODE='1',RUNNER_TRACKING_ID='synthetic-cleanup-tracking',ZALKI_CAPABILITY='synthetic-do-not-forward')
        command=[sys.executable,str(item.layout/'unraced.py'),'--repo',str(item.repo),'--base',fixtures.BASE,'--head',fixtures.HEAD,
                 '--event','pull_request','--tools',str(item.tools),'--output',str(item.output)]
        result=subprocess.run(command,env=env,capture_output=True,text=True,timeout=30)
        self.assertEqual(result.returncode,0,result.stderr)
        proof=json.loads((item.output/'result.json').read_text())
        self.assertEqual(proof['executed_packages'],1);self.assertEqual(proof['coverage']['required_roots'],1)
        self.assertEqual(proof['cli_settings']['vcs.revision'],fixtures.HEAD)
        argv=proof['commands']['tests']['argv'];self.assertIn('-race=false',argv);self.assertIn('-count=1',argv)
        self.assertNotIn('./...',argv);self.assertEqual(argv[-1],PREFIX+'/internal/fixture')


if __name__=='__main__':unittest.main()
