import re
from pathxf import xf
COP="#C2702F"; ESP="#1B130E"; CREAM="#F7ECDD"
_set_symbol_pending=True
wd=re.search(r' d="([^"]+)"',open('word/wu.svg').read()).group(1)
import pickle
from shapely import affinity as _aff
def _bbox(tag):  # measured from the master geometry, never hard-coded
    W,_=pickle.load(open(f'master/{tag}.pkl','rb')); return W.bounds
def _set_symbol(tag):
    global MX0,MY0,MW,MH,symd
    b=_bbox(tag); MX0,MY0,MW,MH=b[0],b[1],b[2]-b[0],b[3]-b[1]
    symd=re.search(r' d="([^"]+)"',open(f'master/{tag}.svg').read()).group(1)
WX0,WX1,BASE,CAP=134.5,2642.0,1000,700   # wordmark bbox x, baseline, cap height (font units at 1000)
def svg(vw,vh,body,title="Gofer"):
    p=2  # safety margin so anti-aliased edges never touch the frame
    return f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="{-p} {-p} {vw+2*p:.2f} {vh+2*p:.2f}" width="{vw+2*p:.0f}" height="{vh+2*p:.0f}" role="img" aria-labelledby="t"><title id="t">{title}</title>{body}</svg>\n'
def horizontal(capr=0.5,gapr=0.22,wcol=ESP,mcol=COP):
    s=capr*MH/CAP; gap=gapr*MH
    m=xf(symd,1,-MX0,-MY0)
    base=MH/2+capr*MH/2
    w=xf(wd,s,MW+gap-WX0*s,base-BASE*s)
    vw=MW+gap+(WX1-WX0)*s
    return svg(vw,MH,f'<path id="symbol" fill="{mcol}" fill-rule="evenodd" d="{m}"/><path id="wordmark" fill="{wcol}" d="{w}"/>')
def stacked(capr=0.36,gapr=0.16,wcol=ESP,mcol=COP):
    s=capr*MH/CAP; ww=(WX1-WX0)*s; vw=max(MW,ww)
    m=xf(symd,1,-MX0+(vw-MW)/2,-MY0)
    capTop=MH+gapr*MH; base=capTop+CAP*s
    w=xf(wd,s,(vw-ww)/2-WX0*s,base-BASE*s)
    vh=base+ (1015-1000)*s
    return svg(vw,vh,f'<path id="symbol" fill="{mcol}" fill-rule="evenodd" d="{m}"/><path id="wordmark" fill="{wcol}" d="{w}"/>')
def wordmark(wcol=ESP):
    s=256/ (1015-277.5); 
    w=xf(wd,s,-WX0*s,-277.5*s)
    return svg((WX1-WX0)*s,256,f'<path id="wordmark" fill="{wcol}" d="{w}"/>')
if __name__=="__main__":
    import os; os.makedirs('lockups',exist_ok=True)
    for c in (0.46,0.52,0.58):
        open(f'lockups/h-{c}.svg','w').write(horizontal(c))
    for c in (0.32,0.38):
        open(f'lockups/s-{c}.svg','w').write(stacked(c))

_set_symbol('gofer-symbol')
