import json,datetime,sys
L='/legion/scratch/tmp/infra594/load/runs/'
n=sys.argv[1]
r=json.load(open(L+n+'.restart.json'))['restart_ns']
def ts(s):
    d=datetime.datetime.fromisoformat(s.replace('Z','+00:00'))
    return int(d.timestamp())*10**9 + int(s.split('.')[1].rstrip('Z').ljust(9,'0')[:9])
turn={};rest={}
for line in open(L+n+'/listener.log'):
    if 'zzdiag' not in line: continue
    j=json.loads(line); t=ts(j['time'])
    if t<r: continue
    if j['msg']=='zzdiag turn': turn.setdefault(j['session_id'],t)
    if j['msg']=='zzdiag restarted': rest.setdefault(j['session_id'],t)
d=sorted((rest[k]-turn[k])/1e6 for k in rest if k in turn)
print('n',len(d),'turn->restarted ms p50/p90/p99/max',[round(d[int(p*(len(d)-1))]) for p in (0.5,0.9,0.99,1)])
print('first turn s', round(min(turn.values())/1e9 - r/1e9,2), 'last restarted', round(max(rest.values())/1e9-r/1e9,2), 'rate/s', round(len(d)/((max(rest.values())-min(turn.values()))/1e9)))
