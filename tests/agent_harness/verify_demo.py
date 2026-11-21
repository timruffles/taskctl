import json
import os
from pathlib import Path
import subprocess
import sys
import shutil

root=Path(sys.argv[1]).resolve()
cfg=json.loads((root/'harness.json').read_text())
env=os.environ.copy();env.update(cfg['environment'])
work=Path(cfg['workdir']);binary=root/'ledger'
go=cfg.get('go') or os.environ.get('TASKCTL_TEST_GO') or shutil.which('go')
if not go:raise RuntimeError('Go is required; set TASKCTL_TEST_GO to its executable')
build=subprocess.run([go,'build','-o',str(binary),'./cmd/ledger'],cwd=work,env=env,text=True,capture_output=True)
if build.returncode:raise RuntimeError(build.stderr)
sample='date,category,amount\n2026-01-05,food,12.34\n2026-01-06,travel,-2.50\n'
expected={'count':2,'total_cents':984,'by_category':{'food':1234,'travel':-250}}
fixtures=root/'fixtures';fixtures.mkdir(exist_ok=True)
(fixtures/'input.csv').write_text(sample)
(fixtures/'-input.csv').write_text(sample)
cases=[
('stdin',[],sample,0,expected),
('dash stdin',['-'],sample,0,expected),
('file',['input.csv'],'',0,expected),
('literal dash file',['--','-input.csv'],'',0,expected),
('header only',[],'date,category,amount\n',0,{'count':0,'total_cents':0,'by_category':{}}),
('positive boundary',[],'date,category,amount\n2026-01-01,a,92233720368547758.07\n',0,{'count':1,'total_cents':9223372036854775807,'by_category':{'a':9223372036854775807}}),
('negative boundary',[],'date,category,amount\n2026-01-01,a,-92233720368547758.08\n',0,{'count':1,'total_cents':-9223372036854775808,'by_category':{'a':-9223372036854775808}}),
('help',['--help'],'',0,None),
('unknown flag',['--bad'],'',2,None),
('help with args',['--help','input.csv'],'',2,None),
('two files',['input.csv','input.csv'],'',2,None),
('missing file',['missing.csv'],'',1,None),
('invalid amount',[],'date,category,amount\n2026-01-01,a,1.001\n',1,None),
('invalid date',[],'date,category,amount\n2026-02-30,a,1\n',1,None),
('padded header',[],' date,category,amount\n2026-01-01,a,1\n',1,None),
('malformed CSV',[],'date,category,amount\n2026-01-01,a\n',1,None),
('total overflow',[],'date,category,amount\n2026-01-01,a,92233720368547758.07\n2026-01-01,b,0.01\n',1,None),
('category overflow with valid net',[],'date,category,amount\n2026-01-01,a,92233720368547758.07\n2026-01-01,b,-0.01\n2026-01-01,a,0.01\n',1,None),
('empty filename',[''],sample,2,None),
('empty then real filename',['','input.csv'],sample,2,None),
('two empty filenames',['',''],sample,2,None),
('literal empty filename',['--',''],sample,2,None),
]
results=[]
for name,args,data,code,value in cases:
    p=subprocess.run([str(binary),*args],input=data,cwd=fixtures,env=env,text=True,capture_output=True)
    good=p.returncode==code
    if code:good=good and not p.stdout and bool(p.stderr)
    elif value is not None:
        try:good=good and json.loads(p.stdout)==value and p.stdout.endswith('\n') and p.stdout.count('\n')==1 and not p.stderr
        except ValueError:good=False
    else:good=good and 'Usage:' in p.stdout and not p.stderr
    results.append({'name':name,'passed':good,'exit':p.returncode,'expected_exit':code,'stdout':p.stdout,'stderr':p.stderr})
result={'passed':all(x['passed'] for x in results),'cases':len(results),'results':results}
dest=root/('demo-fixtures-passed.json' if result['passed'] else 'demo-fixtures-failed.json')
dest.write_text(json.dumps(result,indent=2)+'\n')
print(json.dumps({'passed':result['passed'],'cases':len(results),'failures':[r for r in results if not r['passed']],'evidence':str(dest)}))
sys.exit(0 if result['passed'] else 1)
