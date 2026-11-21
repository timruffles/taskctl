import concurrent.futures
import json
import os
from pathlib import Path
import sqlite3
import subprocess
import sys
import time

root=Path(sys.argv[1]).resolve()
binary=Path(sys.argv[2]).resolve()
measure=Path(sys.argv[3]).resolve()
data=json.loads((root/'cli-results.json').read_text())
env_base=os.environ.copy()
for key in ['GOMEMLIMIT','GODEBUG','GOMAXPROCS']:env_base.pop(key,None)
env_base['GOGC']='100'

def settings(name):
    f=next(x for x in data['fixtures'] if x['name']==name)
    directory=root/name;env=env_base.copy()
    for key,part in [('XDG_DATA_HOME','data'),('XDG_STATE_HOME','state'),('XDG_CONFIG_HOME','config')]:env[key]=str(directory/part)
    env['TASKCTL_PROJECT']=f['project']
    return f,directory,env

def measure_command(directory,env,label,args):
    p=subprocess.run([str(measure),'-timeout','15s','-output',str(directory/(label+'.stdout')),'--',str(binary),*args],env=env,cwd=directory,text=True,capture_output=True,check=True)
    return json.loads(p.stdout)

bursts=[]
for n in [1000,100000]:
    f,directory,env=settings(f'history-{n}')
    with sqlite3.connect(f['database']) as c:claims=c.execute("SELECT id,claim_id FROM tasks WHERE status='in_progress' ORDER BY id LIMIT 8").fetchall()
    assert len(claims)==8
    began=time.monotonic()
    with concurrent.futures.ThreadPoolExecutor(max_workers=8) as pool:
        runs=list(pool.map(lambda row:measure_command(directory,env,'concurrent-'+str(row[0]),['tasks','progress','T'+str(row[0]),'--claim',row[1],'--text','Concurrent verified progress','--json']),claims))
    bursts.append({'tasks':n,'workers':8,'wall_ms':(time.monotonic()-began)*1000,'runs':runs})

result={'concurrent_bursts':bursts}
(root/'edges.json').write_text(json.dumps(result,indent=2)+'\n')
print(json.dumps(result))
