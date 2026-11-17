"""Closed execution profiles and native applicability, never arbitrary argv."""
from checks import require

PROFILES={
    'default':dict(race=True,test_seconds=45,process_seconds=120,build_seconds=300,cli=False,functional_sites=False),
    'bundle_functional':dict(race=True,test_seconds=45,process_seconds=120,build_seconds=300,cli=False,functional_sites=True),
    'r7_background':dict(race=False,test_seconds=120,process_seconds=150,build_seconds=240,cli=True,functional_sites=True),
}


def profile(control):
    name=control.get('profile','default')
    require(type(name) is str and name in PROFILES,'unknown reviewed execution profile')
    rebuild=control.get('rebuild_cli_for_mutant',False)
    require(type(rebuild) is bool and (not rebuild or PROFILES[name]['cli'] and control['kind']=='production'),
            'mutant CLI requires a production CLI execution profile')
    return dict(PROFILES[name])


def native_platforms(control):
    values=control.get('native_os',['darwin','linux'])
    require(type(values) is list and values and all(type(v) is str and v in ['darwin','linux'] for v in values)
            and len(set(values))==len(values),'invalid native control applicability')
    return values


def applicable(control,goos):
    require(goos in ['darwin','linux'],'controls must run on native Darwin/Linux')
    return goos in native_platforms(control)
