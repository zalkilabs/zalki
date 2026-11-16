#!/usr/bin/env python3
"""Compare exact scoped strict diagnostics from both trees and both lint orders."""
import argparse
from collections import Counter
import json
from pathlib import Path, PurePosixPath
import sys


def read_json(path):
    def unique(items):
        value = {}
        for key, entry in items:
            if key in value:
                raise ValueError('duplicate JSON key: ' + key)
            value[key] = entry
        return value
    def nonfinite(value):
        raise ValueError('non-finite JSON value: ' + value)
    return json.loads(Path(path).read_text(), object_pairs_hook=unique, parse_constant=nonfinite)


def read_report(path):
    data = read_json(path)
    if not isinstance(data, list):
        raise ValueError('scoped report must be an array: ' + str(path))
    result = Counter()
    for row in data:
        if not isinstance(row, dict):
            raise ValueError('invalid diagnostic object')
        names = ('linter', 'file', 'scope', 'text', 'source_line')
        if any(type(row.get(name)) is not str or not row[name].strip() for name in names[:4]):
            raise ValueError('diagnostic lacks a literal issue key')
        if type(row.get('source_line')) is not str:
            raise ValueError('diagnostic lacks literal source text')
        file = PurePosixPath(row['file'])
        if file.is_absolute() or '..' in file.parts or str(file) != row['file'] or '\\' in row['file']:
            raise ValueError('diagnostic file is not a repository-relative path')
        result[tuple(row[name] for name in names)] += 1
    return result


def rows(counter):
    return [dict(zip(('linter', 'file', 'scope', 'text', 'source_line', 'count'), (*key, value)))
            for key, value in sorted(counter.items())]


def compare(base_normal, base_strict, head_normal, head_strict):
    base_same = base_normal == base_strict
    head_same = head_normal == head_strict
    added, removed = head_normal - base_normal, base_normal - head_normal
    return dict(passed=base_same and head_same and not added,
                base_diagnostics=sum(base_normal.values()), head_diagnostics=sum(head_normal.values()),
                base_distinct_keys=len(base_normal), head_distinct_keys=len(head_normal),
                base_orders_equal=base_same, head_orders_equal=head_same,
                added=rows(added), removed=rows(removed),
                base_order_only_normal=rows(base_normal - base_strict),
                base_order_only_strict=rows(base_strict - base_normal),
                head_order_only_normal=rows(head_normal - head_strict),
                head_order_only_strict=rows(head_strict - head_normal))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ['base-normal-first', 'base-strict-first', 'head-normal-first', 'head-strict-first']:
        parser.add_argument('--' + name, required=True, type=Path)
    options = parser.parse_args()
    try:
        result = compare(*(read_report(getattr(options, name)) for name in
                           ['base_normal_first', 'base_strict_first', 'head_normal_first', 'head_strict_first']))
    except (OSError, ValueError) as error:
        print('strict comparison input failure: ' + str(error), file=sys.stderr)
        return 2
    print(json.dumps(result, sort_keys=True))
    return 0 if result['passed'] else 1


if __name__ == '__main__':
    sys.exit(main())
