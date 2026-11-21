#!/usr/bin/env python3
"""Local command harness with installed asynchronous taskctl hooks and scoped faults."""
import argparse
import concurrent.futures
import contextlib
import json
import os
from pathlib import Path
import shlex
import signal
import sqlite3
import subprocess
import sys
import time
import threading

SCHEMA = '''
CREATE TABLE IF NOT EXISTS agents(name TEXT PRIMARY KEY,started REAL NOT NULL,crashed INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS commands(id INTEGER PRIMARY KEY,agent TEXT NOT NULL,argv TEXT NOT NULL,status TEXT NOT NULL DEFAULT 'pending',pid INTEGER,started REAL NOT NULL,finished REAL,exit_code INTEGER,stdout TEXT,stderr TEXT);
CREATE TABLE IF NOT EXISTS jobs(id INTEGER PRIMARY KEY,agent TEXT NOT NULL,event TEXT NOT NULL,input TEXT NOT NULL,status TEXT NOT NULL DEFAULT 'pending',started REAL,finished REAL,output TEXT NOT NULL DEFAULT '',error TEXT NOT NULL DEFAULT '',ready_at REAL NOT NULL DEFAULT 0,delivered INTEGER NOT NULL DEFAULT 0,pid INTEGER);
CREATE TABLE IF NOT EXISTS faults(id INTEGER PRIMARY KEY,agent TEXT NOT NULL,kind TEXT NOT NULL,value TEXT NOT NULL,remaining INTEGER NOT NULL DEFAULT 1);
CREATE TABLE IF NOT EXISTS controls(id INTEGER PRIMARY KEY,kind TEXT NOT NULL,agent TEXT NOT NULL,done INTEGER NOT NULL DEFAULT 0,result TEXT);
CREATE TABLE IF NOT EXISTS audit(id INTEGER PRIMARY KEY,at REAL NOT NULL,kind TEXT NOT NULL,agent TEXT NOT NULL,data TEXT NOT NULL);
'''

def db(root):
    c=sqlite3.connect(root/'harness.sqlite',timeout=5,isolation_level=None)
    c.row_factory=sqlite3.Row
    c.execute('PRAGMA busy_timeout=5000')
    c.execute('PRAGMA journal_mode=WAL')
    c.executescript(SCHEMA)
    return c

def audit(c,kind,agent,data):
    c.execute('INSERT INTO audit(at,kind,agent,data) VALUES(?,?,?,?)',(time.time(),kind,agent,json.dumps(data)))

def config(root):
    return json.loads((root/'harness.json').read_text())

def environment(cfg):
    env=os.environ.copy();env.update(cfg['environment'])
    return env

def enqueue(root,agent,event,command=None,response=None):
    cfg=config(root)
    payload={'hook_event_name':event,'session_id':'chaos-root','agent_id':agent,'cwd':cfg['workdir']}
    if event=='PostToolUse':
        payload.update(tool_name='Bash',tool_input={'command':command},tool_response=response)
    with contextlib.closing(db(root)) as c:
        c.execute('INSERT INTO jobs(agent,event,input) VALUES(?,?,?)',(agent,event,json.dumps(payload)))

def consume_fault(c,agent,kind):
    c.execute('BEGIN IMMEDIATE')
    r=c.execute('SELECT id,value FROM faults WHERE agent=? AND kind=? AND remaining>0 ORDER BY id LIMIT 1',(agent,kind)).fetchone()
    if r:c.execute('UPDATE faults SET remaining=remaining-1 WHERE id=?',(r['id'],))
    c.execute('COMMIT')
    return r['value'] if r else None

def drain(root,agent):
    with contextlib.closing(db(root)) as c:
        c.execute('BEGIN IMMEDIATE')
        rows=c.execute("SELECT id,event,output FROM jobs WHERE agent=? AND status='done' AND delivered=0 AND ready_at<=? AND output<>'' ORDER BY ready_at,id",(agent,time.time())).fetchall()
        result=[]
        for r in rows:
            try:obj=json.loads(r['output']);text=obj['hookSpecificOutput']['additionalContext']
            except (ValueError,KeyError,TypeError):text=r['output']
            result.append({'job_id':r['id'],'event':r['event'],'additional_context':text})
            c.execute('UPDATE jobs SET delivered=1 WHERE id=?',(r['id'],))
            audit(c,'context_delivered',agent,{'job_id':r['id'],'bytes':len(text.encode())})
        c.execute('COMMIT')
        return result

def execute_hook(root,jobid):
    cfg=config(root)
    with contextlib.closing(db(root)) as c:
        r=c.execute('SELECT * FROM jobs WHERE id=?',(jobid,)).fetchone()
        event=r['event'];agent=r['agent']
        if c.execute('SELECT crashed FROM agents WHERE name=?',(agent,)).fetchone()['crashed']:
            audit(c,'hook_revoked',agent,{'job_id':jobid})
            c.execute("UPDATE jobs SET status='dropped',finished=? WHERE id=?",(time.time(),jobid));return
        installed=json.loads((root/'agents'/agent/'hooks.json').read_text())
        command=installed['hooks'][event][0]['hooks'][0]['command']
        if consume_fault(c,agent,'skip_hook') is not None:
            audit(c,'hook_skipped',agent,{'job_id':jobid})
            c.execute("UPDATE jobs SET status='dropped',finished=? WHERE id=?",(time.time(),jobid));return
        delay=consume_fault(c,agent,'delay_hook')
        if delay is not None:time.sleep(float(delay))
        p=subprocess.Popen(shlex.split(command),stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.PIPE,text=True,env=environment(cfg),cwd=cfg['workdir'])
        c.execute('UPDATE jobs SET pid=? WHERE id=?',(p.pid,jobid))
        try:out,err=p.communicate(r['input'],timeout=10)
        except subprocess.TimeoutExpired:
            p.kill();out,err=p.communicate();err+='\nhook timed out'
        audit(c,'hook_executed',agent,{'job_id':jobid,'event':event,'exit':p.returncode,'stdout_bytes':len(out.encode())})
        if out:
            if consume_fault(c,agent,'drop_context') is not None:
                audit(c,'context_dropped',agent,{'job_id':jobid,'bytes':len(out.encode())})
                c.execute("UPDATE jobs SET status='dropped',finished=?,output=?,error=? WHERE id=?",(time.time(),out,err,jobid));return
            delayed=consume_fault(c,agent,'delay_context')
            ready=time.time()+float(delayed or 0)
            if delayed is not None:audit(c,'context_delayed',agent,{'job_id':jobid,'seconds':float(delayed)})
            duplicate=consume_fault(c,agent,'duplicate_context')
        else:ready=time.time();duplicate=None
        c.execute("UPDATE jobs SET status='done',finished=?,output=?,error=?,ready_at=? WHERE id=?",(time.time(),out,err,ready,jobid))
        if duplicate is not None:
            c.execute("INSERT INTO jobs(agent,event,input,status,started,finished,output,error,ready_at) VALUES(?,?,?,'done',?,?,?,?,?)",(agent,event,r['input'],time.time(),time.time(),out,err,ready+float(duplicate or 0)))
            audit(c,'context_duplicated',agent,{'job_id':jobid})


CHILDREN={}
CHILDREN_LOCK=threading.Lock()

class RevokedAgent(RuntimeError):
    pass

def execute_command(root,cid):
    cfg=config(root)
    with contextlib.closing(db(root)) as c:
        r=c.execute('SELECT * FROM commands WHERE id=?',(cid,)).fetchone();agent=r['agent'];argv=json.loads(r['argv'])
        with CHILDREN_LOCK:
            if c.execute('SELECT crashed FROM agents WHERE name=?',(agent,)).fetchone()['crashed']:
                raise RevokedAgent('Agent execution is revoked; only an explicit controller restart can resume commands.')
            p=subprocess.Popen(argv,cwd=cfg['workdir'],env=environment(cfg),stdout=subprocess.PIPE,stderr=subprocess.PIPE,text=True,errors='replace',start_new_session=True)
            CHILDREN[cid]=(agent,p)
        c.execute('UPDATE commands SET pid=? WHERE id=?',(p.pid,cid))
        try:
            out,err=p.communicate(timeout=180)
        except subprocess.TimeoutExpired:
            with CHILDREN_LOCK:
                if p.poll() is None:os.killpg(p.pid,signal.SIGKILL)
            out,err=p.communicate();err+='\ncommand timed out'
        finally:
            with CHILDREN_LOCK:CHILDREN.pop(cid,None)
        out=out if len(out)<=65536 else out[:65536]+'\n[Harness output truncated after 65536 characters.]\n'
        err=err if len(err)<=65536 else err[:65536]+'\n[Harness error output truncated after 65536 characters.]\n'
        visible={'exit':p.returncode,'stdout':out,'stderr':err}
        c.execute("UPDATE commands SET status='done',finished=?,exit_code=?,stdout=?,stderr=? WHERE id=?",(time.time(),p.returncode,out,err,cid))
        enqueue(root,agent,'PostToolUse',shlex.join(argv),visible)

def serve(root):
    root.mkdir(parents=True,exist_ok=True)
    with contextlib.closing(db(root)) as c:
        c.execute("UPDATE jobs SET status='pending',pid=NULL WHERE status='running'")
        c.execute("UPDATE commands SET status='done',finished=?,exit_code=74,stdout='',stderr='Harness restarted; command outcome unknown. Inspect state before retrying.' WHERE status='running'",(time.time(),))
        audit(c,'server_started','controller',{'pid':os.getpid()})
    (root/'server.pid').write_text(str(os.getpid()))
    pool=concurrent.futures.ThreadPoolExecutor(max_workers=8);running={};commands={}
    try:
        while not (root/'stop-server').exists():
            for mapping,table in [(running,'jobs'),(commands,'commands')]:
                for jid,f in list(mapping.items()):
                    if f.done():
                        try:f.result()
                        except Exception as e:
                            with contextlib.closing(db(root)) as c:
                                if table=='jobs':c.execute("UPDATE jobs SET status='error',finished=?,error=? WHERE id=?",(time.time(),str(e),jid))
                                else:
                                    c.execute("UPDATE commands SET status='done',finished=?,exit_code=75,stdout='',stderr=? WHERE id=?",(time.time(),str(e),jid))
                                    row=c.execute('SELECT agent,argv FROM commands WHERE id=?',(jid,)).fetchone()
                                    if not isinstance(e,RevokedAgent):
                                        enqueue(root,row['agent'],'PostToolUse',shlex.join(json.loads(row['argv'])),{'exit':75,'stdout':'','stderr':str(e)})
                        del mapping[jid]
            with contextlib.closing(db(root)) as c:
                controls=c.execute('SELECT * FROM controls WHERE done=0 ORDER BY id').fetchall()
                for control in controls:
                    if control['kind']=='crash':
                        killed=[]
                        with CHILDREN_LOCK:
                            c.execute('UPDATE agents SET crashed=1 WHERE name=?',(control['agent'],))
                            for cid,(agent,process) in CHILDREN.items():
                                if agent==control['agent'] and process.poll() is None:
                                    os.killpg(process.pid,signal.SIGKILL);killed.append(process.pid)
                        result={'agent':control['agent'],'crashed':True,'killed_owned_command_pids':killed}
                        audit(c,'agent_crashed',control['agent'],result)
                        c.execute('UPDATE controls SET done=1,result=? WHERE id=?',(json.dumps(result),control['id']))
                    elif control['kind']=='service_crash':
                        with CHILDREN_LOCK:
                            if CHILDREN:continue
                        audit(c,'server_sigkill','controller',{'pid':os.getpid()})
                        c.execute('UPDATE controls SET done=1,result=? WHERE id=?',(json.dumps({'server_crashed':True}),control['id']))
                        os.kill(os.getpid(),signal.SIGKILL)
                c.execute('BEGIN IMMEDIATE')
                jobs=c.execute("SELECT id FROM jobs WHERE status='pending' ORDER BY id LIMIT ?",(4-len(running),)).fetchall()
                for r in jobs:c.execute("UPDATE jobs SET status='running',started=? WHERE id=?",(time.time(),r['id']))
                cmds=c.execute("SELECT id FROM commands WHERE status='pending' ORDER BY id LIMIT ?",(4-len(commands),)).fetchall()
                for r in cmds:c.execute("UPDATE commands SET status='running' WHERE id=?",(r['id'],))
                c.execute('COMMIT')
            for r in jobs:running[r['id']]=pool.submit(execute_hook,root,r['id'])
            for r in cmds:commands[r['id']]=pool.submit(execute_command,root,r['id'])
            time.sleep(.03)
    finally:pool.shutdown(wait=True)

def run(root,agent,argv):
    if argv and argv[0]=='--':argv=argv[1:]
    if not argv:raise ValueError('run needs a command after --')
    with contextlib.closing(db(root)) as c:
        registered=c.execute('SELECT crashed FROM agents WHERE name=?',(agent,)).fetchone()
        if registered is None:raise ValueError('agent hooks have not been installed/started')
        if registered['crashed']:
            audit(c,'command_rejected',agent,{'reason':'execution_revoked','argv':argv})
            raise RevokedAgent('Agent execution is revoked; only an explicit controller restart can resume commands.')
        contexts=drain(root,agent)
        cur=c.execute('INSERT INTO commands(agent,argv,started) VALUES(?,?,?)',(agent,json.dumps(argv),time.time()));cid=cur.lastrowid
        deadline=time.time()+200
        while True:
            r=c.execute('SELECT * FROM commands WHERE id=?',(cid,)).fetchone()
            if r['status']=='done':break
            if time.time()>deadline:raise TimeoutError('harness command deadline; outcome unknown')
            time.sleep(.05)
        if r['exit_code']==0 and consume_fault(c,agent,'drop_command_response') is not None:
            audit(c,'command_response_dropped',agent,{'command_id':cid,'argv':argv,'real_exit':r['exit_code']})
            visible={'exit':74,'stdout':'','stderr':'Injected transport failure: command outcome is unknown. Read state or retry with the SAME --request-id.\n'}
        else:visible={'exit':r['exit_code'],'stdout':r['stdout'],'stderr':r['stderr']}
    contexts.extend(drain(root,agent))
    print(json.dumps({'command_id':cid,**visible,'hook_context':contexts}))
    return 0

def main():
    p=argparse.ArgumentParser();p.add_argument('--root',required=True,type=Path);sub=p.add_subparsers(dest='op',required=True)
    sub.add_parser('serve')
    st=sub.add_parser('start');st.add_argument('--agent',required=True)
    rr=sub.add_parser('run');rr.add_argument('--agent',required=True);rr.add_argument('argv',nargs=argparse.REMAINDER)
    dr=sub.add_parser('drain');dr.add_argument('--agent',required=True)
    fa=sub.add_parser('fault');fa.add_argument('--agent',required=True);fa.add_argument('--kind',required=True);fa.add_argument('--value',default='0');fa.add_argument('--count',type=int,default=1)
    sub.add_parser('service-crash');sub.add_parser('status');ex=sub.add_parser('export');ex.add_argument('--file',required=True,type=Path)
    cr=sub.add_parser('crash');cr.add_argument('--agent',required=True)
    a=p.parse_args();root=a.root.resolve()
    if a.op=='serve':return serve(root)
    if a.op=='run':return run(root,a.agent,a.argv)
    if a.op=='drain':print(json.dumps({'hook_context':drain(root,a.agent)}));return
    with contextlib.closing(db(root)) as c:
        if a.op=='start':
            cfg=config(root);dest=root/'agents'/a.agent;dest.mkdir(parents=True,exist_ok=True)
            command=subprocess.run([cfg['binary'],'--project',cfg['project_id'],'integrations','codex','config'],env=environment(cfg),text=True,capture_output=True,check=True)
            hooks=json.loads(command.stdout)
            for event in hooks['hooks']:
                for group in hooks['hooks'][event]:
                    for h in group['hooks']:
                        h['command']+=' --lease 10s --limit 3 --max-context-bytes 1800'
            (dest/'hooks.json').write_text(json.dumps(hooks,indent=2)+'\n')
            c.execute('INSERT INTO agents(name,started,crashed) VALUES(?,?,0) ON CONFLICT(name) DO UPDATE SET started=excluded.started,crashed=0',(a.agent,time.time()))
            audit(c,'hooks_installed',a.agent,{'config':str(dest/'hooks.json'),'events':list(hooks['hooks'])})
            enqueue(root,a.agent,'SessionStart');print(json.dumps({'agent':a.agent,'hooks':str(dest/'hooks.json')}))
        elif a.op=='fault':
            allowed={'skip_hook','delay_hook','drop_context','delay_context','duplicate_context','drop_command_response'}
            if a.kind not in allowed or a.count<1:raise ValueError('invalid scoped fault')
            c.execute('INSERT INTO faults(agent,kind,value,remaining) VALUES(?,?,?,?)',(a.agent,a.kind,a.value,a.count));audit(c,'fault_armed',a.agent,{'kind':a.kind,'value':a.value,'count':a.count})
            print(json.dumps({'armed':a.kind,'agent':a.agent}))
        elif a.op in ['crash','service-crash']:
            agent=a.agent if a.op=='crash' else 'controller'
            kind='crash' if a.op=='crash' else 'service_crash'
            cur=c.execute('INSERT INTO controls(kind,agent) VALUES(?,?)',(kind,agent));cid=cur.lastrowid
            deadline=time.time()+15
            while time.time()<deadline:
                r=c.execute('SELECT done,result FROM controls WHERE id=?',(cid,)).fetchone()
                if r['done']:print(r['result']);break
                time.sleep(.05)
            else:raise TimeoutError('control request queued; server unavailable or not idle')
        elif a.op=='status':
            print(json.dumps({'agents':[dict(r) for r in c.execute('SELECT * FROM agents')],'commands_running':[dict(r) for r in c.execute('SELECT id,agent,pid,argv FROM commands WHERE finished IS NULL')],'jobs':[dict(r) for r in c.execute('SELECT status,COUNT(*) AS count FROM jobs GROUP BY status')],'faults':[dict(r) for r in c.execute('SELECT * FROM faults')]}))
        elif a.op=='export':
            data={table:[dict(r) for r in c.execute('SELECT * FROM '+table+' ORDER BY rowid')] for table in ['agents','commands','jobs','faults','controls','audit']};a.file.write_text(json.dumps(data,indent=2)+'\n');print(json.dumps({'file':str(a.file)}))

if __name__=='__main__':
    try:sys.exit(main() or 0)
    except Exception as e:print(json.dumps({'error':str(e)}),file=sys.stderr);sys.exit(1)
