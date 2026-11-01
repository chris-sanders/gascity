import os, sys, shutil, time, asyncio, json, statistics, collections, threading, subprocess
from pathlib import Path
kind=sys.argv[1]; root=Path('/tmp/final01-clean-'+kind); shutil.rmtree(root,ignore_errors=True); root.mkdir()
os.environ.update(DATABASE_URL='sqlite+aiosqlite:///'+str(root/'mail.sqlite3'),STORAGE_ROOT=str(root/'storage'),APP_ENVIRONMENT='test',GIT_AUTHOR_NAME='Test',GIT_AUTHOR_EMAIL='test@example.com',TOOLS_LOG_ENABLED='false')
from contextlib import asynccontextmanager
from fastmcp import Client
from mcp_agent_mail.app import build_mcp_server
import mcp_agent_mail.app as app
import mcp_agent_mail.storage as storage
import git, git.index.fun as fun
import importlib.metadata as metadata, platform, re
names={re.sub(r"[-_.]+", "-", d.metadata.get("Name", "")).lower() for d in metadata.distributions() if d.metadata.get("Name")}
packages={name:metadata.version(name) for name in sorted(names)}
print('EFFECTIVE_PACKAGES',json.dumps(packages,sort_keys=True),flush=True)
print('EXPERIMENT_ENV',json.dumps({'python':sys.version,'platform':platform.platform(),'gitpython':git.__version__,'gitpython_file':git.__file__,'git_executable':subprocess.check_output(['git','--version'],text=True).strip()}),flush=True)
archive_waits=[]
archive_lock=app.archive_write_lock
@asynccontextmanager
async def timed_archive(*a,**kw):
 t=time.perf_counter(); acquired=False
 try:
  async with archive_lock(*a,**kw):
   archive_waits.append(time.perf_counter()-t)
   acquired=True
   yield
 except TimeoutError:
  if not acquired: archive_waits.append(time.perf_counter()-t)
  raise
app.archive_write_lock=timed_archive
commits=[]; lock=threading.Lock(); calls=0; elapsed=0
orig=storage._commit_direct
async def timed(*a,**kw):
 t=time.perf_counter()
 try: return await orig(*a,**kw)
 finally: commits.append(time.perf_counter()-t)
storage._commit_direct=timed
async def main():
 server=build_mcp_server(); errors=[]; ids=[]; request_latencies=[]; service_latencies=[]; sem=asyncio.Semaphore(8)
 async with Client(server) as c:
  p=(await c.call_tool('ensure_project',{'human_key':'/tmp/burst-project'})).data; print('project',p,flush=True); slug=p['slug']
  a=(await c.call_tool('register_agent',{'project_key':slug,'program':'codex','model':'test','name':'BlueLake'})).data; print('agent',a.get('name'),flush=True)
  repo=root/'storage'
  def index_stats():
   files=subprocess.check_output(['git','-C',str(repo),'ls-files','-z'])
   return {'entries':len(files.split(b'\0'))-1, 'index_bytes':(repo/'.git/index').stat().st_size}
  print('INDEX_INITIAL',json.dumps(index_stats()),flush=True)
  commits.clear(); archive_waits.clear()
  async def send(i):
   queued_at=time.perf_counter()
   async with sem:
    service_at=time.perf_counter()
    try:
     r=await c.call_tool('send_message',{'project_key':slug,'sender_name':'BlueLake','to':['BlueLake'],'subject':f'burst-{i:04}','body_md':f'message {i}'})
     ids.append((i,r.data))
    except Exception as e: errors.append((i,type(e).__name__,str(e)))
    finally:
     finished_at=time.perf_counter()
     request_latencies.append(finished_at-queued_at)
     service_latencies.append(finished_at-service_at)
  t=time.perf_counter(); await asyncio.gather(*(send(i) for i in range(400))); wall=time.perf_counter()-t
  print('INDEX_FINAL',json.dumps(index_stats()),flush=True)
  inbox=await c.call_tool('fetch_inbox',{'project_key':slug,'agent_name':'BlueLake','limit':500})
  inbox_rows=inbox.data or []; print('INBOX_SUMMARY',json.dumps({'count':len(inbox_rows),'unique_ids':len({x.get('id') for x in inbox_rows}),'unique_subjects':len({x.get('subject') for x in inbox_rows})},default=str),flush=True)
  def pct(v,q): return sorted(v)[min(len(v)-1,int(len(v)*q))] if v else None
  print('REQUEST_LATENCY',json.dumps({'request_count':len(request_latencies),'service_count':len(service_latencies),'request_p50':pct(request_latencies,.5),'request_p95':pct(request_latencies,.95),'request_max':max(request_latencies,default=None),'request_over_60s':sum(x>60 for x in request_latencies),'service_p50':pct(service_latencies,.5),'service_p95':pct(service_latencies,.95),'service_max':max(service_latencies,default=None),'service_over_60s':sum(x>60 for x in service_latencies)}),flush=True)
  print('RESULT',json.dumps({'kind':kind,'gitpython':git.__version__,'wall':wall,'success':len(ids),'errors':errors,'unique_requests':len({x[0] for x in ids}),'response_sample':ids[0][1] if ids else None,'archive_wait_count':len(archive_waits),'archive_wait_over_60s':sum(x>60 for x in archive_waits),'archive_wait_p50':pct(archive_waits,.5),'archive_wait_p95':pct(archive_waits,.95),'archive_wait_max':max(archive_waits,default=None),'commit_count':len(commits),'commit_over_60s':sum(x>60 for x in commits),'commit_p50':pct(commits,.5),'commit_p95':pct(commits,.95),'commit_max':max(commits,default=None)},default=str),flush=True)
asyncio.run(main())
