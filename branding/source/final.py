import math, json
from shapely.geometry import Point, Polygon, LineString, box
from shapely.ops import unary_union
from shapely import affinity
from bezfit import geom_to_path
COP="#C2702F"; BROWN="#7E3F16"; CREAM="#F7ECDD"; ESP="#1B130E"
C=lambda x,y,r: Point(x,y).buffer(r,quad_segs=256)
def superellipse(cx,cy,a,b,n,rot=0,N=2048):
    pts=[]
    for i in range(N):
        t=2*math.pi*i/N; c,s=math.cos(t),math.sin(t)
        pts.append((a*math.copysign(abs(c)**(2/n),c), b*math.copysign(abs(s)**(2/n),s)))
    p=affinity.rotate(Polygon(pts),rot,origin=(0,0)); return affinity.translate(p,cx,cy)
P1,P2=4.6889,2.0303  # lobe phases (seed 363457)
def wax(cx=128,cy=128,R=90,N=2048):
    pts=[]
    for i in range(N):
        t=2*math.pi*i/N; r=R+4.5*math.sin(5*t+P1)+2.5*math.sin(3*t+P2)
        pts.append((cx+r*math.cos(t),cy+r*math.sin(t)))
    return Polygon(pts)
MJ=dict(join_style=2,mitre_limit=10,quad_segs=256)
def stamp_geom(ea,cx,cy,R=56,inset=3,es=17,n=2.2,w=7,cw=8,flapA=28):
    ears=[]
    for a in (-90-ea,-90+ea):
        d=R-inset; ears.append(superellipse(cx+d*math.cos(math.radians(a)),cy+d*math.sin(math.radians(a)),es,es,n,rot=a))
    body=C(cx,cy,R); sh=unary_union([body]+ears)
    pp=lambda a:(cx+R*math.cos(math.radians(a)),cy+R*math.sin(math.radians(a)))
    L,T=pp(180+flapA),(cx,cy+10)
    ci=C(cx,cy,R-w/2).exterior; ei=ears[0].buffer(-w/2,**MJ).exterior
    pts=ci.intersection(ei); pts=list(getattr(pts,'geoms',[pts]))
    J=max(((q.x,q.y) for q in pts),key=lambda q:q[1])
    dx,dy=T[0]-L[0],T[1]-L[1]; sd=((J[0]-L[0])*dy-(J[1]-L[1])*dx)/math.hypot(dx,dy)
    return ears,sh,L,T,J,sd
def solve(cx,cy,**kw):
    cw=kw.get('cw',8); lo,hi=36,52
    f=lambda ea: stamp_geom(ea,cx,cy,**kw)[5]-cw/2
    flo=f(lo)
    for _ in range(50):
        m=(lo+hi)/2; fm=f(m)
        if (fm>0)==(flo>0): lo,flo=m,fm
        else: hi=m
    return (lo+hi)/2
def stamp(cx,cy,R=56,w=7,cw=8,**kw):
    ea=solve(cx,cy,R=R,w=w,cw=cw,**kw)
    ears,sh,L,T,J,sd=stamp_geom(ea,cx,cy,R=R,w=w,cw=cw,**kw)
    line=sh.buffer(w/2,**MJ).difference(sh.buffer(-w/2,**MJ))
    ext=(L[0]-(T[0]-L[0])*0.3,L[1]-(T[1]-L[1])*0.3)
    chev=LineString([ext,T,(2*cx-ext[0],ext[1])]).buffer(cw/2,cap_style=2,join_style=2,mitre_limit=10)
    chev=chev.intersection(sh.buffer(w/2,**MJ))
    ear_int=unary_union([e.buffer(-w/2,**MJ) for e in ears]).difference(C(cx,cy,R-w/2))
    chev=chev.difference(ear_int)
    return unary_union([line,chev]),ea,sd,J
def build(small=False):
    W=wax()
    # optical centring: put the stamp's centre a touch above the wax's centre of mass
    kw=dict(w=13,cw=13,es=18,inset=2) if small else {}
    s0,_,_,_=stamp(128,136,**kw); b=s0.bounds; sc=((b[0]+b[2])/2,(b[1]+b[3])/2)
    wc=W.centroid; target=(wc.x, wc.y-2)
    cy=136+(target[1]-sc[1])
    S,ea,sd,J=stamp(128,cy,**kw); S=affinity.translate(S,wc.x-128,0)
    # centre the whole mark on the 256 canvas
    bb=W.bounds; dx,dy=128-(bb[0]+bb[2])/2,128-(bb[1]+bb[3])/2
    T=lambda g: affinity.translate(g,dx,dy)
    return T(W),T(S),dict(ears_deg=round(ea,3),flap_edge_offset=round(sd,4),stamp_cy=round(cy+dy,2))
HDR='<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 256 256" width="256" height="256" role="img" aria-labelledby="t"><title id="t">Gofer</title>'
if __name__=="__main__":
    import os; os.makedirs("master",exist_ok=True); info={}
    for small in (False,True):
        W,S,meta=build(small); tag="gofer-symbol-small" if small else "gofer-symbol"; info[tag]=meta
        cut=W.difference(S)
        open(f"master/{tag}.svg","w").write(HDR+f'<path id="symbol" fill="{COP}" fill-rule="evenodd" d="{geom_to_path(cut)}"/></svg>\n')
        open(f"master/{tag}-pressed.svg","w").write(HDR+f'<path id="wax" fill="{COP}" d="{geom_to_path(W)}"/><path id="stamp" fill="{BROWN}" fill-rule="evenodd" d="{geom_to_path(S.intersection(W))}"/></svg>\n')
        # polygon reference for verification
        def poly_d(g):
            out=""
            for p in getattr(g,'geoms',[g]):
                for r in [p.exterior,*p.interiors]: out+="M"+"L".join(f"{x:.3f} {y:.3f}" for x,y in list(r.coords)[:-1])+"Z"
            return out
        open(f"master/{tag}-polyref.svg","w").write(HDR+f'<path fill="#000" fill-rule="evenodd" d="{poly_d(cut)}"/></svg>\n')
        import pickle; pickle.dump((W,S),open(f"master/{tag}.pkl","wb"))
    json.dump(info,open("master/geometry.json","w"),indent=1); print(json.dumps(info,indent=1))
