#!/usr/bin/env python3
"""Exact architecture changes against a base-bound, reviewed allowance ledger."""
import json
from pathlib import Path
import re

CATEGORIES = ('exports', 'shapes', 'receivers', 'imports', 'os_calls')


def require(condition, message):
    if not condition:
        raise ValueError(message)


def read_json(path):
    def unique(items):
        result = {}
        for key, value in items:
            require(key not in result, 'duplicate JSON key: ' + key)
            result[key] = value
        return result
    def nonfinite(value):
        raise ValueError('non-finite JSON value: ' + value)
    return json.loads(Path(path).read_text(), object_pairs_hook=unique, parse_constant=nonfinite)


def string_set(values, name):
    require(type(values) is list and all(type(x) is str and x for x in values), name + ' must be a string array')
    require(len(values) == len(set(values)), name + ' contains duplicate identities')
    return sorted(values)


def validate(snapshot):
    require(type(snapshot) is dict and type(snapshot.get('build')) is dict and type(snapshot.get('facts')) is dict,
            'missing build/facts snapshot')
    build, facts = snapshot['build'], snapshot['facts']
    require(build.get('GOOS') in ['darwin', 'linux'], 'snapshot must come from a native Darwin/Linux job')
    require(all(type(build.get(k)) is str and build[k] for k in ['GOARCH', 'GOVERSION', 'CGO_ENABLED']),
            'incomplete build identity')
    require(build['CGO_ENABLED'] in ['0','1'], 'invalid CGO build identity')
    require(set(facts) == set(CATEGORIES), 'architecture fact categories differ')
    result = dict(exports=string_set(facts['exports'], 'exports'), imports=string_set(facts['imports'], 'imports'))
    for kind in ['shapes', 'receivers', 'os_calls']:
        require(type(facts[kind]) is dict and all(type(k) is str and k for k in facts[kind]), 'invalid ' + kind)
        result[kind] = facts[kind]
    require(all(type(v) is str and v for v in result['shapes'].values()), 'invalid literal API shape')
    shaped = []
    for key in result['shapes']:
        kind, separator, name = key.partition(':')
        require(separator and kind in ['function','type','const','var'] and name, 'invalid API shape identity')
        shaped.append(name)
    require(sorted(shaped) == result['exports'], 'named exports and declaration shapes differ')
    for value in result['receivers'].values():
        require(type(value) is dict and set(value) == {'Methods', 'Files'} and
                all(type(n) is int and n >= 0 for n in value.values()), 'invalid receiver count')
    result['os_calls'] = {k:string_set(v, 'raw OS imports') for k,v in result['os_calls'].items()}
    exceptions = string_set(snapshot.get('os_call_exceptions'), 'raw OS exception paths')
    require(exceptions == sorted(result['os_calls']), 'raw OS exceptions and actual users differ')
    # Preserve literal keys/shape strings; ordering of sets/maps carries no budget.
    return build, result


def mapping_delta(before, after):
    return dict(added={k:after[k] for k in sorted(after.keys()-before.keys())},
                removed={k:before[k] for k in sorted(before.keys()-after.keys())},
                changed={k:dict(before=before[k],after=after[k]) for k in sorted(before.keys()&after.keys())
                         if before[k] != after[k]})


def calculate(before, after):
    result = {}
    for kind in CATEGORIES:
        if kind in ['exports','imports']:
            old,new = set(before[kind]),set(after[kind])
            result[kind] = dict(added=sorted(new-old), removed=sorted(old-new))
        else:
            result[kind] = mapping_delta(before[kind],after[kind])
    return result


def has_changes(delta):
    return any(items for category in delta.values() for items in category.values())


def compare(base_commit, before, after, approvals):
    require(type(base_commit) is str and re.fullmatch('[0-9a-f]{40}',base_commit) is not None and base_commit != '0'*40,
            'actual base commit is required')
    old_build, old = validate(before)
    new_build, new = validate(after)
    require(old_build == new_build, 'base/head build identities differ')
    require(type(approvals) is list, 'reviewed approvals must be an array')
    delta = calculate(old,new)
    matches = []
    for entry in approvals:
        require(type(entry) is dict and set(entry) == {'base_commit','os','delta','reason'}, 'invalid reviewed allowance')
        require(type(entry['base_commit']) is str and re.fullmatch('[0-9a-f]{40}',entry['base_commit']) is not None,
                'allowance lacks an exact base')
        require(entry['os'] in ['darwin','linux'] and type(entry['reason']) is str and entry['reason'].strip(),
                'allowance lacks an OS/review reason')
        if entry['base_commit'] == base_commit and entry['os'] == new_build['GOOS'] and json.dumps(entry['delta'],sort_keys=True,allow_nan=False) == json.dumps(delta,sort_keys=True,allow_nan=False):
            matches.append(entry)
    require(len(matches) <= 1, 'ambiguous duplicate reviewed allowance')
    changed = has_changes(delta)
    return dict(passed=not changed or len(matches)==1, actual_base=base_commit, build=new_build, delta=delta,
                export_count_before=len(old['exports']), export_count_after=len(new['exports']),
                approval_used=matches[0]['reason'] if changed and matches else None,
                source='exact current base/head measurements; no numeric growth credit')
