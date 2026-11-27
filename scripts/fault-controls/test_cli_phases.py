"""Pure CLI receipt/phase binding; synthetic binary hashes are not native proof."""
import copy
import unittest
import oracle
import test_event_states as events

HEAD='b'*40
PLATFORM=dict(GOVERSION='fixture-go',GOOS='linux',GOARCH='fixture',CGO_ENABLED='1')
BASE=dict(files=2,sha256='a'*64)
BAD=dict(files=2,sha256='b'*64)


def record(mutant):
    settings=dict(GOOS='linux',GOARCH='fixture',CGO_ENABLED='1',**{'vcs.revision':HEAD,'vcs.modified':str(mutant).lower()})
    return dict(head=HEAD,modified=mutant,source_fingerprint=BAD if mutant else BASE,binary_sha256=('b' if mutant else 'a')*64,
                build=dict(exit=0,log_sha256='c'*64,argv=['go','build','-trimpath','-race=false','-buildvcs=true','./cmd/zalki']),
                metadata=dict(exit=0,log_sha256='d'*64),settings=settings,
                build_info=dict(Path='github.com/zalkilabs/zalki/cmd/zalki',GoVersion='fixture-go',Settings=[dict(Key=k,Value=v) for k,v in settings.items()]))


def fixture():
    value=events.fixture();value['cli_policy']=dict(head=HEAD,platform=PLATFORM,baseline_fingerprint=BASE,rebuild_mutant=True)
    for phase in ['good','intended','restored']:
        value['phases'][phase]['source_fingerprint']=BAD if phase=='intended' else BASE
        value['phases'][phase]['cli']=record(phase=='intended')
    return value


def invoke(context):
    return oracle.validate(context,dict(package=events.PKG,test=events.ROOT),events.identity('PRIMARY_MARKER'),
                           events.attestation('PRIMARY_MARKER'),'PRIMARY_MARKER',cli_required=True,mutant_cli_required=True)


class PhaseCLI(unittest.TestCase):
    def test_real_CLI_policy_is_required_and_mutant_cannot_reuse_baseline(self):
        context=fixture();proof=invoke(context)
        self.assertNotEqual(proof['phase_CLI_sha256']['good'],proof['phase_CLI_sha256']['intended'])
        absent=copy.deepcopy(context);del absent['cli_policy']
        with self.assertRaisesRegex(ValueError,'required phase CLI policy is missing'):invoke(absent)
        downgraded=copy.deepcopy(context);downgraded['cli_policy']['rebuild_mutant']=False;downgraded['phases']['intended']['cli']=copy.deepcopy(downgraded['phases']['good']['cli'])
        with self.assertRaisesRegex(ValueError,'differs from recipe'):invoke(downgraded)
        stale=copy.deepcopy(context);stale['phases']['intended']['cli']=copy.deepcopy(stale['phases']['good']['cli'])
        with self.assertRaisesRegex(ValueError,'phase source'):invoke(stale)
        unchanged=copy.deepcopy(context);unchanged['phases']['intended']['source_fingerprint']=BASE
        with self.assertRaisesRegex(ValueError,'still the clean baseline'):invoke(unchanged)

    def test_CLI_raw_revision_native_race_and_restored_hash_stay_exact(self):
        changes=[lambda c:c['phases']['intended']['cli']['build_info']['Settings'].append(dict(Key='-race',Value='true')),
                 lambda c:c['phases']['good']['cli']['build_info']['Settings'].append(dict(Key='vcs.revision',Value='a'*40)),
                 lambda c:c['phases']['good']['cli']['build_info']['Settings'].append(dict(Key='GOOS',Value='darwin')),
                 lambda c:c['phases']['restored']['cli'].update(binary_sha256='0'*64),
                 lambda c:c['phases']['good']['cli']['build'].update(exit=2),
                 lambda c:c['phases']['good']['cli']['build'].update(argv=['go','build','./cmd/zalki']),
                 lambda c:c['phases']['good']['cli']['build'].update(argv=['go','build','-race=false','-buildvcs=true','./cmd/zalki']),  # no -trimpath
                 lambda c:c['phases']['good']['cli']['build'].update(argv=[]),
                 lambda c:c['phases']['good'].pop('cli'),
                 lambda c:c['cli_policy'].update(baseline_fingerprint=None)]
        for change in changes:
            context=fixture();change(context)
            with self.subTest(change=change),self.assertRaises(ValueError):invoke(context)
