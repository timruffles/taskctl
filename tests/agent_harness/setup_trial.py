import json
import os
from pathlib import Path
import subprocess
import sys
import shutil

repo=Path(__file__).resolve().parents[2]
root=Path(sys.argv[1]).resolve()
root.mkdir(parents=True,exist_ok=False)
work=repo/'examples'/'ledger'
env=os.environ.copy()
for key,part in [('XDG_DATA_HOME','data'),('XDG_STATE_HOME','state'),('XDG_CONFIG_HOME','config')]:
    env[key]=str(root/part)
go=os.environ.get('TASKCTL_TEST_GO') or shutil.which('go')
if not go:raise RuntimeError('Go is required; set TASKCTL_TEST_GO to its executable')
go=str(Path(go).resolve())
for part in ['tmp','go-cache']:(root/part).mkdir()
env.update(TMPDIR=str(root/'tmp'),GOCACHE=str(root/'go-cache'),GOGC='30',GOMEMLIMIT='700MiB',GOFLAGS='-buildvcs=false -p=1')
binary=repo/'bin'/'taskctl'
def call(*args):
    r=subprocess.run([str(binary),*args,'--json'],cwd=work,env=env,text=True,capture_output=True)
    if r.returncode:raise RuntimeError(r.stderr)
    return json.loads(r.stdout)
project=call('projects','create','--name','ledger-chaos')
project_id=project['id']
env['TASKCTL_PROJECT']=project_id
conf=Path(env['XDG_CONFIG_HOME'])/'taskctl'/'config.json'
conf.parent.mkdir(parents=True,exist_ok=True)
conf.write_text(json.dumps({'inactivity':'20s','scratch_grace':'1s','list_limit':20})+'\n')
top=call('tasks','create','--title','Build exact-cent CSV ledger','--brief','Implement the package and CLI in CONTRACT.md; preserve exact integer cents and reject malformed input.','--tag','demo')
owner=call('tasks','claim',top['id'],'--session','controller')
plan={'summary':'Contract and file ownership agreed in examples/ledger/CONTRACT.md. Three independent implementation branches remain.','replacements':[
{'key':'amount','title':'Implement exact cents parser','brief':'Implement ParseCents in amount.go against CONTRACT.md. Store important code in this repository. Record a checkpoint before the planned worker interruption.','tags':['amount']},
{'key':'csv','title':'Implement CSV input and checked totals','brief':'Implement shared Transaction and Summary types, ReadCSV and Summarize from CONTRACT.md. Own csv.go, csv_test.go, summary.go, summary_test.go. Test CSV errors and separate category/total overflow.','tags':['csv']},
{'key':'cli','title':'Implement ledger command','brief':'Implement cmd/ledger main and tests against CONTRACT.md. Own cmd/ledger only initially. Subscribe to original T1; recover a confirmed crashed amount worker with exact-token takeover.','tags':['cli']} ]}
planfile=root/'initial-plan.json';planfile.write_text(json.dumps(plan))
split=call('tasks','evolve',top['id'],'--claim',owner['claim_id'],'--revision',str(owner['revision']),'--file',str(planfile))
integration=call('tasks','create','--title','Verify recovered ledger end to end','--brief','Run all race tests and actual CLI fixtures, then verify task lineage, ownership fencing and notification acknowledgements.','--prerequisite',top['id'],'--tag','integration')
subs={name:call('tasks','subscribe',top['id'],'--session',name) for name in ['chaos-a','chaos-b','chaos-c']}
cfg={'binary':str(binary),'go':go,'project_id':project_id,'workdir':str(work),'environment':{k:env[k] for k in ['XDG_DATA_HOME','XDG_STATE_HOME','XDG_CONFIG_HOME','TASKCTL_PROJECT','TMPDIR','GOCACHE','GOGC','GOMEMLIMIT','GOFLAGS']}}
(root/'harness.json').write_text(json.dumps(cfg,indent=2)+'\n')
(root/'setup-evidence.json').write_text(json.dumps({'project':project,'original':top,'split':split,'integration':integration,'subscriptions':subs},indent=2)+'\n')
print(json.dumps({'project':project_id,'root':str(root),'split':split,'subscriptions':subs}))
