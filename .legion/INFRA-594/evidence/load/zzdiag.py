import json,csv,sys,datetime,statistics
L='/legion/scratch/tmp/infra594/load/runs/'
n=sys.argv[1]
rs=json.load(open(L+n+'.restart.json')); r=rs['restart_ns']
bl=json.load(open(L+n+'.backlog.json'))
turn={};restarted={};pulled={}
def ts(s): 
    d=datetime.datetime.fromisoformat(s.replace('Z','+00:00'))
    return int(d.timestamp())*10**9 + int(s.split('.')[1].rstrip('Z').ljust(9,'0')[:9])
for line in open(L+n+'/listener.log'):
    if 'zzdiag' not in line: continue
    j=json.loads(line); t=ts(j['time'])
    if t<r: continue
    sid=j['session_id']
    if j['msg']=='zzdiag turn': turn.setdefault(sid,t)
    elif j['msg']=='zzdiag restarted': restarted.setdefault(sid,t)
    elif j['msg']=='zzdiag pulled': pulled[(sid,int(j['seq']))]=t
recv={}
for row in csv.DictReader(open(L+n+'/relay-frames.csv')):
    e=int(row['event_id'])
    if bl['id_from']<=e<=bl['id_to'] and row['duplicate']=='false':
        recv[(row['session'],int(row['stream_seq']))]=int(row['recv_ns'])
s=lambda v:(v-r)/1e9
d_pull=[];d_recv=[];d_first=[]
last_by={}
for k,t in recv.items():
    sid=k[0]
    if k in pulled:
        d_recv.append((t-pulled[k])/1e6)
    last_by[sid]=max(last_by.get(sid,0),t)
firstpull={}
for (sid,seq),t in pulled.items():
    if sid in restarted: firstpull[sid]=min(firstpull.get(sid,1<<62),t)
for sid,t in firstpull.items(): d_first.append((t-restarted[sid])/1e6)
def q(a): a=sorted(a); return [round(a[int(p*(len(a)-1))],1) for p in (0.5,0.9,0.99,1.0)] if a else None
print('turn s', q([s(v) for v in turn.values()]))
print('restarted s', q([s(v) for v in restarted.values()]))
print('restart->first pulled ms', q(d_first))
print('pulled->received ms', q(d_recv))
print('session last recv s', q([s(v) for v in last_by.values()]))
# for the 20 slowest sessions
slow=sorted(last_by.items(), key=lambda x:-x[1])[:8]
for sid,t in slow:
    ps=sorted((seq,v) for (ss,seq),v in pulled.items() if ss==sid)
    print(sid,'turn',round(s(turn.get(sid,r)),2),'restarted',round(s(restarted.get(sid,r)),2),'pulled',[round(s(v),2) for _,v in ps][-4:],'last recv',round(s(t),2))
