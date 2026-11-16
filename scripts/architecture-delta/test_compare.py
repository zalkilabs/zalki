import copy
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest

spec=importlib.util.spec_from_file_location('architecture_compare',Path(__file__).with_name('budget.py'))
module=importlib.util.module_from_spec(spec);spec.loader.exec_module(module)
BASE='a'*40


class ExactArchitectureBudgets(unittest.TestCase):
    def setUp(self):
        self.base=dict(build=dict(GOOS='linux',GOARCH='amd64',GOVERSION='fixture-go',CGO_ENABLED='1'),
                       facts=dict(exports=['internal/example.Public'],shapes={'type:internal/example.Public':'struct { Value string }'},
                                  receivers={'internal/example.Store':dict(Methods=2,Files=1)},imports=[],os_calls={}),
                       os_call_exceptions=[])

    def add_export(self,facts,name):
        facts['exports'].append(name)
        facts['shapes']['type:'+name]='struct{}'

    def approval(self,head,base=BASE):
        _,old=module.validate(self.base);_,new=module.validate(head)
        return dict(base_commit=base,os='linux',delta=copy.deepcopy(module.calculate(old,new)),reason='Reviewed exact API change')

    def test_equal_reports_need_no_fixed_counts_or_allowance(self):
        result=module.compare(BASE,self.base,copy.deepcopy(self.base),[])
        self.assertTrue(result['passed']);self.assertIsNone(result['approval_used'])

    def test_added_removed_and_changed_shapes_require_exact_review(self):
        edits=[lambda f:self.add_export(f,'internal/example.Other'),
               lambda f:(f['exports'].clear(),f['shapes'].clear()),
               lambda f:f['shapes'].update({'type:internal/example.Public':'struct { Value int }'}),
               lambda f:f['receivers']['internal/example.Store'].update(Methods=3),
               lambda f:f['imports'].append('internal/example -> internal/model')]
        for edit in edits:
            with self.subTest(edit=edit):
                head=copy.deepcopy(self.base);edit(head['facts'])
                self.assertFalse(module.compare(BASE,self.base,head,[])['passed'])
                self.assertTrue(module.compare(BASE,self.base,head,[self.approval(head)])['passed'])

    def test_approved_count_does_not_cover_a_different_symbol_or_type(self):
        head=copy.deepcopy(self.base);self.add_export(head['facts'],'internal/example.One')
        approval=self.approval(head)
        head['facts']['exports'][-1]='internal/example.Two'
        head['facts']['shapes']['type:internal/example.Two']=head['facts']['shapes'].pop('type:internal/example.One')
        self.assertFalse(module.compare(BASE,self.base,head,[approval])['passed'])
        head=copy.deepcopy(self.base);head['facts']['shapes']['type:internal/example.Public']='struct { Value int }'
        approval=self.approval(head);head['facts']['shapes']['type:internal/example.Public']='struct { Value bool }'
        self.assertFalse(module.compare(BASE,self.base,head,[approval])['passed'])

    def test_stale_base_or_other_os_cannot_supply_review_credit(self):
        head=copy.deepcopy(self.base);self.add_export(head['facts'],'internal/example.New')
        old=self.approval(head,base='b'*40)
        self.assertFalse(module.compare(BASE,self.base,head,[old])['passed'])
        other=self.approval(head);other['os']='darwin'
        self.assertFalse(module.compare(BASE,self.base,head,[other])['passed'])

    def test_raw_os_exception_list_must_match_the_measured_users(self):
        head=copy.deepcopy(self.base);head['facts']['os_calls']['internal/example/process.go']=['syscall']
        with self.assertRaises(ValueError):module.compare(BASE,self.base,head,[])
        head['os_call_exceptions']=['internal/example/process.go']
        self.assertFalse(module.compare(BASE,self.base,head,[])['passed'])
        self.assertTrue(module.compare(BASE,self.base,head,[self.approval(head)])['passed'])
        head['os_call_exceptions'].append('internal/example/unused.go')
        with self.assertRaises(ValueError):module.compare(BASE,self.base,head,[])

    def test_duplicate_identities_and_broad_or_duplicate_approvals_refuse(self):
        head=copy.deepcopy(self.base);head['facts']['exports'].append('internal/example.Public')
        with self.assertRaises(ValueError):module.compare(BASE,self.base,head,[])
        head=copy.deepcopy(self.base);self.add_export(head['facts'],'internal/example.New');entry=self.approval(head)
        with self.assertRaises(ValueError):module.compare(BASE,self.base,head,[entry,entry])
        entry['maximum_added_exports']=1
        with self.assertRaises(ValueError):module.compare(BASE,self.base,head,[entry])

    def test_platform_change_or_bool_receiver_count_is_not_comparable(self):
        head=copy.deepcopy(self.base);head['build']['CGO_ENABLED']='0'
        with self.assertRaises(ValueError):module.compare(BASE,self.base,head,[])
        head=copy.deepcopy(self.base);head['facts']['receivers']['internal/example.Store']['Files']=True
        with self.assertRaises(ValueError):module.compare(BASE,self.base,head,[])

    def test_boolean_in_reviewed_counts_cannot_alias_integer_budget(self):
        head=copy.deepcopy(self.base);head['facts']['receivers']['internal/example.Store']['Methods']=1
        entry=self.approval(head)
        entry['delta']['receivers']['changed']['internal/example.Store']['after']['Methods']=True
        self.assertFalse(module.compare(BASE,self.base,head,[entry])['passed'])

    def test_partial_shape_coverage_is_not_a_complete_snapshot(self):
        for kind in ['missing','extra','wrong-kind']:
            with self.subTest(kind=kind):
                head=copy.deepcopy(self.base)
                if kind=='missing':head['facts']['shapes'].clear()
                elif kind=='extra':head['facts']['shapes']['type:internal/example.Unknown']='struct{}'
                else:head['facts']['shapes']={'unknown:internal/example.Public':'struct{}'}
                with self.assertRaises(ValueError):module.compare(BASE,self.base,head,[])

    def test_json_reader_refuses_duplicate_or_partial_data(self):
        with tempfile.TemporaryDirectory() as directory:
            path=Path(directory)/'input.json'
            for data in ['{','{"facts":{},"facts":{}}','NaN']:
                path.write_text(data)
                with self.assertRaises(ValueError):module.read_json(path)


if __name__=='__main__':unittest.main()
