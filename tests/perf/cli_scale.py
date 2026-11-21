import argparse
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import platform
import sqlite3
import statistics
import shutil
import subprocess
import time

p=argparse.ArgumentParser()
p.add_argument('--root',required=True,type=Path)
p.add_argument('--binary',required=True,type=Path)
p.add_argument('--measure',required=True,type=Path)
p.add_argument('--sizes',default='100,1000,10000,100000')
a=p.parse_args();root=a.root.resolve();root.mkdir(parents=True,exist_ok=True)
binary=a.binary.resolve();measure=a.measure.resolve()
base=os.environ.copy()
for key in ['GOMEMLIMIT','GODEBUG','GOMAXPROCS']:base.pop(key,None)
base['GOGC']='100'
stamp=datetime.now(timezone.utc).strftime('%Y-%m-%dT%H:%M:%S.000000000Z')
results=[];fixtures=[]

def fixture(name,n,events_per_task=10):
    directory=root/name;directory.mkdir()
    env=base.copy()
    for key,part in [('XDG_DATA_HOME','data'),('XDG_STATE_HOME','state'),('XDG_CONFIG_HOME','config')]:env[key]=str(directory/part)
    p=subprocess.run([str(binary),'projects','create','--name',name,'--json'],env=env,cwd=directory,text=True,capture_output=True,check=True)
    project=json.loads(p.stdout);env['TASKCTL_PROJECT']=project['id']
    db=Path(env['XDG_DATA_HOME'])/'taskctl/projects'/project['id']/'tasks.sqlite'
    c=sqlite3.connect(db);c.execute('PRAGMA foreign_keys=ON')
    began=time.monotonic();c.execute('BEGIN')
    def tasks():
        for i in range(1,n+1):
            status='completed' if i<=n-20 else ('pending' if i<=n-5 else 'in_progress')
            owner=f'perf-{i-(n-4)}' if status=='in_progress' else ''
            claim=f'Cperf{i-(n-4)}' if owner else ''
            yield (i,f'Task {i}','b'*1024,status,i,owner,claim,'Checkpoint '+str(i),'Done' if status=='completed' else '',stamp,stamp,stamp if owner else '')
    c.executemany('INSERT INTO tasks(id,title,brief,status,origin_id,owner,claim_id,progress,result,created_at,updated_at,last_activity_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)',tasks())
    c.executemany('INSERT INTO prerequisites(task_id,prerequisite_id) VALUES(?,1)',((i,) for i in range(n-19,n-4)))
    c.executemany('INSERT INTO tags(task_id,tag) VALUES(?,?)',((i,'fixture') for i in range(1,n+1)))
    c.executemany('INSERT INTO metadata(task_id,key,value) VALUES(?,?,?)',((i,'session_reference','previous-session') for i in range(1,n+1)))
    event_data=json.dumps({'fixture':True,'note':'x'*256},separators=(',',':'))
    c.executemany('INSERT INTO events(task_id,kind,message,data,created_at) VALUES(?,?,?,?,?)',((i,'progress','Meaningful checkpoint '+str(k),event_data,stamp) for i in range(1,n+1) for k in range(events_per_task)))
    high=c.execute('SELECT max(id) FROM events').fetchone()[0]
    c.execute("INSERT INTO subscriptions(id,task_id,session,cursor,snapshot_pending,queue_cursor,queue_snapshot_pending,created_at,updated_at) VALUES('Sperf',1,'perf-4',?,0,?,0,?,?)",(high,high,stamp,stamp))
    c.commit();c.execute('PRAGMA wal_checkpoint(TRUNCATE)');c.close()
    description={'name':name,'tasks':n,'active':20,'events':n*events_per_task,'bytes':db.stat().st_size,'fixture_seconds':time.monotonic()-began,'database':str(db),'environment':env,'project':project['id']}
    fixtures.append({k:v for k,v in description.items() if k!='environment'})
    return directory,env,db

def run_case(name,operation,argv,env,directory,reps=3,input_data=None,timeout="15s"):
    stdin=directory/'stdin.json'
    if input_data is not None:stdin.write_text(json.dumps(input_data))
    runs=[]
    for i in range(reps):
        cmd=[str(measure),'-output',str(directory/'stdout.tmp'),'-timeout',timeout]
        if input_data is not None:cmd+=['-input',str(stdin)]
        args=argv(i) if callable(argv) else argv
        p=subprocess.run(cmd+['--',str(binary),*args],env=env,cwd=directory,text=True,capture_output=True,check=True)
        result=json.loads(p.stdout);runs.append(result)
        if result['exit'] or result['timed_out']:break
    result={'fixture':name,'operation':operation,'runs':runs,'median_ms':statistics.median(r['elapsed_ms'] for r in runs),'max_rss_mib':max(r['peak_rss_bytes'] for r in runs)/1048576}
    results.append(result)
    (root/'cli-results.json').write_text(json.dumps({'method':'Fresh CLI process; first-use included; 3 diagnostic repetitions; GOGC100; no race/GOMEMLIMIT; Go supervisor wait4 peak RSS','environment':{'platform':platform.platform(),'cpu_count':os.cpu_count()},'fixtures':fixtures,'results':results},indent=2)+'\n')
    print(json.dumps({k:v for k,v in result.items() if k!='runs'}),flush=True)
    return runs

for n in map(int,a.sizes.split(',')):
    name=f'history-{n}';directory,env,db=fixture(name,n)
    run_case(name,'get',['tasks','show',f'T{n}','--json'],env,directory)
    run_case(name,'in-progress',['tasks','in-progress','--json'],env,directory)
    run_case(name,'available',['tasks','available','--limit','20','--json'],env,directory)
    run_case(name,'snapshot-one',['tasks','watch','T1','--once','--json'],env,directory)
    run_case(name,'progress',['tasks','progress',f'T{n}','--claim','Cperf4','--text','New verified checkpoint','--json'],env,directory)
    run_case(name,'claim',lambda i:['tasks','claim',f'T{n-19+i}','--session','perf-claim','--json'],env,directory)
    hook={'hook_event_name':'PostToolUse','session_id':'perf-4','agent_id':'perf-4','cwd':str(directory),'tool_name':'Bash','tool_input':{'command':'true'},'tool_response':{'exit_code':0}}
    run_case(name,'quiet-hook',['integrations','codex','hook'],env,directory,input_data=hook)
    run_case(name,'sqlite-backup',['backup','--file',str(directory/'backup.sqlite'),'--json'],env,directory,reps=1,timeout='120s')
    fixtures[-1]['backup_bytes']=(directory/'backup.sqlite').stat().st_size
    if n>=10000:
        runs=run_case(name,'sqlite-restore',['restore','--file',str(directory/'backup.sqlite'),'--name','perf-restored','--json'],env,directory,reps=1,timeout='120s')
        if runs[-1]['exit']==0:
            restored=json.loads((directory/'stdout.tmp').read_text())
            restored_db=Path(env['XDG_DATA_HOME'])/'taskctl/projects'/restored['id']/'tasks.sqlite'
            with sqlite3.connect(restored_db) as c:
                checked={'integrity':c.execute('PRAGMA quick_check').fetchone()[0], 'tasks':c.execute('SELECT count(*) FROM tasks').fetchone()[0], 'events':c.execute('SELECT count(*) FROM events').fetchone()[0], 'claims':c.execute("SELECT count(*) FROM tasks WHERE claim_id<>''").fetchone()[0], 'subscriptions':c.execute('SELECT count(*) FROM subscriptions').fetchone()[0]}
                assert checked['integrity']=='ok' and checked['tasks']==n and checked['claims']==0 and checked['subscriptions']==0
            (directory/'restore-validation.json').write_text(json.dumps(checked,indent=2)+'\n')
            shutil.rmtree(restored_db.parent)

directory,env,db=fixture('lineage-1000',1000,1)
with sqlite3.connect(db) as c:
    c.execute('DELETE FROM prerequisites')
    c.execute("UPDATE tasks SET status='superseded',owner='',claim_id='',origin_id=1,parent_id=CASE WHEN id=1 THEN NULL ELSE id-1 END WHERE id<1000")
    c.execute("UPDATE tasks SET parent_id=999,origin_id=1 WHERE id=1000")
    c.execute("INSERT INTO tasks(id,title,status,origin_id,created_at,updated_at) VALUES(1001,'Blocked dependent','pending',1001,?,?)",(stamp,stamp))
    c.execute('INSERT INTO prerequisites(task_id,prerequisite_id) VALUES(1001,1)')
run_case('lineage-1000','progress',['tasks','progress','T1000','--claim','Cperf4','--text','Verified progress','--json'],env,directory,reps=1)
run_case('lineage-1000','snapshot-leaf',['tasks','watch','T1000','--once','--json'],env,directory,reps=1)
run_case('lineage-1000','history-page',['tasks','history','T1000','--json'],env,directory,reps=1)

directory,env,db=fixture('unrelated-tail',100,1)
hook={'hook_event_name':'PostToolUse','session_id':'perf-4','agent_id':'perf-4','cwd':str(directory),'tool_name':'Bash','tool_input':{'command':'true'}}
added=0
for target in [0,10000,100000,500000]:
    if target>added:
        with sqlite3.connect(db) as c:
            c.executemany("INSERT INTO events(task_id,kind,message,data,created_at) VALUES(2,'progress','Unrelated progress','{}',?)",((stamp,) for _ in range(target-added)))
        added=target
    run_case(f'unrelated-tail-{target}','quiet-hook',['integrations','codex','hook'],env,directory,input_data=hook)

directory,env,db=fixture('quiet-subscriptions',1000,1)
for count in [1,10,100,1000]:
    with sqlite3.connect(db) as c:
        current=c.execute('SELECT COUNT(*) FROM subscriptions').fetchone()[0]
        high=c.execute('SELECT max(id) FROM events').fetchone()[0]
        c.executemany('INSERT INTO subscriptions(id,task_id,session,cursor,snapshot_pending,queue_cursor,queue_snapshot_pending,created_at,updated_at) VALUES(?,?,?,?,0,?,0,?,?)',((f'Sperf{i}',i,'perf-4',high,high,stamp,stamp) for i in range(current+1,count+1)))
    run_case(f'quiet-subscriptions-{count}','quiet-hook',['integrations','codex','hook'],env,directory,input_data=hook)
