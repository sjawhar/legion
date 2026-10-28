import re,sys,glob
def cat(stack):
    if 'sessionRelays).restart' in stack:
        if 'streamState' in stack: k='restart.streamState'
        elif 'keepAtFloor' in stack and 'update' in stack: k='restart.update'
        elif 'SessionConsumers).info' in stack: k='restart.info'
        elif 'SessionConsumers).create' in stack: k='restart.create'
        elif 'SessionConsumers).delete' in stack: k='restart.delete'
        elif 'SessionConsumers).Pull' in stack: k='restart.pullinfo'
        elif 'sessionLocks).lock' in stack: k='restart.lock'
        elif 'session_relay.go:48' in stack: k='restart.turn'
        else: k='restart.other'
        if 'FlushTimeout' in stack: k+='(flush)'
        elif 'NextMsg' in stack or 'requestWithContext' in stack: k+='(reply)'
        return k
    if 'SessionPull).Next' in stack: return 'relay.Next'
    if 'register' in stack and 'sessionDelivery' in stack: return 'registration'
    return None
for f in sorted(glob.glob(sys.argv[1]+'/gor.*.txt'), key=lambda x:int(x.split('.')[-2])):
    txt=open(f).read()
    groups=re.split(r'\n\n', txt)
    c={}
    for g in groups:
        m=re.search(r'^(\d+) @', g, re.M)
        if not m: continue
        k=cat(g)
        if k: c[k]=c.get(k,0)+int(m.group(1))
    print(f.split('/')[-1], sorted(c.items()))
