import re
def xf(d,s,tx,ty):
    """Scale+translate SVG path data (no rotation); handles MLHVCSQTAZ abs/rel."""
    toks=re.findall(r'[MmLlHhVvCcSsQqTtAaZz]|-?(?:\d+\.?\d*|\.\d+)(?:[eE][-+]?\d+)?',d)
    out=[]; i=0; cmd=None
    n={'M':2,'L':2,'T':2,'H':1,'V':1,'C':6,'S':4,'Q':4,'A':7,'Z':0}
    while i<len(toks):
        t=toks[i]
        if re.match(r'[A-Za-z]',t): cmd=t; out.append(t); i+=1
        if cmd.upper()=='Z': continue
        k=n[cmd.upper()]; vals=[float(v) for v in toks[i:i+k]]; i+=k
        rel=cmd.islower(); U=cmd.upper(); r=[]
        if U=='H': r=[vals[0]*s+(0 if rel else tx)]
        elif U=='V': r=[vals[0]*s+(0 if rel else ty)]
        elif U=='A': r=[vals[0]*s,vals[1]*s,vals[2],vals[3],vals[4],vals[5]*s+(0 if rel else tx),vals[6]*s+(0 if rel else ty)]
        else:
            for j in range(0,k,2): r+= [vals[j]*s+(0 if rel else tx), vals[j+1]*s+(0 if rel else ty)]
        out.append(" ".join(f"{v:.3f}".rstrip('0').rstrip('.') if isinstance(v,float) else str(v) for v in r))
        if U=='M': cmd='l' if rel else 'L'
    return " ".join(out)
