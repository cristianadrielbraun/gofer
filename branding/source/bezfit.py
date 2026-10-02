"""Fit dense polygon rings with cubic Béziers (Schneider), keeping sharp corners exactly."""
import math
def _sub(a,b): return (a[0]-b[0],a[1]-b[1])
def _add(a,b): return (a[0]+b[0],a[1]+b[1])
def _mul(a,s): return (a[0]*s,a[1]*s)
def _dot(a,b): return a[0]*b[0]+a[1]*b[1]
def _norm(a):
    l=math.hypot(*a); return (a[0]/l,a[1]/l) if l else (0.0,0.0)
def _bez(c,t):
    mt=1-t; return (mt**3*c[0][0]+3*mt*mt*t*c[1][0]+3*mt*t*t*c[2][0]+t**3*c[3][0],
                    mt**3*c[0][1]+3*mt*mt*t*c[1][1]+3*mt*t*t*c[2][1]+t**3*c[3][1])
def _bezd(c,t):
    mt=1-t; return (3*mt*mt*(c[1][0]-c[0][0])+6*mt*t*(c[2][0]-c[1][0])+3*t*t*(c[3][0]-c[2][0]),
                    3*mt*mt*(c[1][1]-c[0][1])+6*mt*t*(c[2][1]-c[1][1])+3*t*t*(c[3][1]-c[2][1]))
def _bezdd(c,t):
    mt=1-t; return (6*mt*(c[2][0]-2*c[1][0]+c[0][0])+6*t*(c[3][0]-2*c[2][0]+c[1][0]),
                    6*mt*(c[2][1]-2*c[1][1]+c[0][1])+6*t*(c[3][1]-2*c[2][1]+c[1][1]))
def _chord(pts):
    u=[0.0]
    for i in range(1,len(pts)): u.append(u[-1]+math.dist(pts[i],pts[i-1]))
    return [x/u[-1] for x in u] if u[-1] else u
def _gen(pts,u,t1,t2):
    p0,p3=pts[0],pts[-1]
    C=[[0,0],[0,0]]; X=[0,0]
    for p,t in zip(pts,u):
        mt=1-t; a1=_mul(t1,3*mt*mt*t); a2=_mul(t2,3*mt*t*t)
        C[0][0]+=_dot(a1,a1); C[0][1]+=_dot(a1,a2); C[1][1]+=_dot(a2,a2)
        tmp=_sub(p,_add(_add(_mul(p0,mt**3),_mul(p0,3*mt*mt*t)),_add(_mul(p3,3*mt*t*t),_mul(p3,t**3))))
        X[0]+=_dot(a1,tmp); X[1]+=_dot(a2,tmp)
    C[1][0]=C[0][1]
    det=C[0][0]*C[1][1]-C[0][1]*C[1][0]
    seg=math.dist(p0,p3)
    if abs(det)>1e-12:
        al=(X[0]*C[1][1]-X[1]*C[0][1])/det; ar=(C[0][0]*X[1]-C[1][0]*X[0])/det
    else: al=ar=seg/3
    eps=1e-6*seg
    if al<eps or ar<eps: al=ar=seg/3
    return [p0,_add(p0,_mul(t1,al)),_add(p3,_mul(t2,ar)),p3]
def _maxerr(pts,c,u):
    m,idx=0.0,len(pts)//2
    for i,(p,t) in enumerate(zip(pts,u)):
        d=math.dist(_bez(c,t),p)
        if d>m: m,idx=d,i
    return m,idx
def _reparam(pts,c,u):
    out=[]
    for p,t in zip(pts,u):
        d=_sub(_bez(c,t),p); d1=_bezd(c,t); d2=_bezdd(c,t)
        num=_dot(d,d1); den=_dot(d1,d1)+_dot(d,d2)
        out.append(min(1,max(0,t-num/den)) if abs(den)>1e-12 else t)
    return out
def _fit(pts,t1,t2,tol,out):
    if len(pts)==2:
        d=math.dist(*pts)/3; out.append([pts[0],_add(pts[0],_mul(t1,d)),_add(pts[1],_mul(t2,d)),pts[1]]); return
    u=_chord(pts); c=_gen(pts,u,t1,t2); err,idx=_maxerr(pts,c,u)
    if err<tol: out.append(c); return
    if err<tol*4:
        for _ in range(20):
            u=_reparam(pts,c,u); c=_gen(pts,u,t1,t2); err,idx=_maxerr(pts,c,u)
            if err<tol: out.append(c); return
    idx=max(1,min(len(pts)-2,idx))
    tc=_norm(_sub(pts[idx-1],pts[idx+1]))
    _fit(pts[:idx+1],t1,tc,tol,out); _fit(pts[idx:],_mul(tc,-1),t2,tol,out)
def _straight(run,tol):
    a,b=run[0],run[-1]; L=math.dist(a,b)
    if L<1e-9: return False
    return all(abs((p[0]-a[0])*(b[1]-a[1])-(p[1]-a[1])*(b[0]-a[0]))/L<tol for p in run)
def ring_to_path(coords,tol=0.04,corner_deg=12):
    pts=[]
    for p in coords[:-1] if coords[0]==coords[-1] else coords:
        if not pts or math.dist(p,pts[-1])>1e-6: pts.append(p)
    if math.dist(pts[0],pts[-1])<1e-6: pts.pop()
    n=len(pts)
    def turn(i):
        a,b,c=pts[i-1],pts[i],pts[(i+1)%n]
        v1,v2=_norm(_sub(b,a)),_norm(_sub(c,b))
        return math.degrees(math.acos(max(-1,min(1,_dot(v1,v2)))))
    corners=[i for i in range(n) if turn(i)>corner_deg]
    if not corners:  # smooth closed curve: start anywhere, use periodic tangents
        corners=[0]; smooth_closed=True
    else: smooth_closed=False
    d=f"M{pts[corners[0]][0]:.3f} {pts[corners[0]][1]:.3f}"
    k=len(corners)
    for j in range(k):
        s=corners[j]; e=corners[(j+1)%k]
        run=[pts[(s+i)%n] for i in range(((e-s)%n or n)+1)]
        if _straight(run,tol):
            d+=f"L{run[-1][0]:.3f} {run[-1][1]:.3f}"; continue
        if smooth_closed:
            t1=_norm(_sub(run[1],run[-2])); t2=_mul(t1,-1)
        else:
            t1=_norm(_sub(run[1],run[0])); t2=_norm(_sub(run[-2],run[-1]))
        segs=[]; _fit(run,t1,t2,tol,segs)
        for c in segs:
            if _straight([c[0],_bez(c,.25),_bez(c,.5),_bez(c,.75),c[3]],tol/4):
                d+=f"L{c[3][0]:.3f} {c[3][1]:.3f}"
            else:
                d+="C"+" ".join(f"{q[0]:.3f} {q[1]:.3f}" for q in c[1:])
    return d+"Z"
def geom_to_path(geom,tol=0.04):
    out=[]
    for poly in getattr(geom,'geoms',[geom]):
        for r in [poly.exterior,*poly.interiors]: out.append(ring_to_path(list(r.coords),tol))
    return "".join(out)
