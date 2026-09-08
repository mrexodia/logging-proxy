// Incremental decoders: network chunks need not align with UTF-8, SSE lines,
// HTTP headers, or WebSocket frames. Raw bytes always remain in the log files.
export function fromBase64(value) { return Uint8Array.from(atob(value), c => c.charCodeAt(0)); }
export function hex(bytes) { return Array.from(bytes,b=>b.toString(16).padStart(2,'0')).join(' '); }
export function pretty(text) { try { return JSON.stringify(JSON.parse(text),null,2); } catch { return text; } }

export class SSEDecoder {
  constructor(emit) { this.emit=emit; this.line=''; this.cr=false; this.reset(); this.id=''; }
  reset() { this.data=[]; this.dataLength=0; this.type='message'; this.hasData=false; }
  push(text) {
    for(const c of text) {
      if(this.cr) { this.cr=false; if(c==='\n') continue; }
      if(c==='\r'||c==='\n') { this.process(this.line); this.line=''; this.cr=c==='\r'; }
      else { this.line+=c; if(this.line.length>2*1024*1024) { this.emit('Large SSE line','Preview limit reached; use the raw capture.'); this.line=''; this.reset(); } }
    }
  }
  process(line) {
    if(!line) { if(this.hasData) this.emit(this.type+(this.id?' · '+this.id:''),this.data.join('\n')); this.reset(); return; }
    if(line.startsWith(':')) return;
    const colon=line.indexOf(':'); const field=colon<0?line:line.slice(0,colon); let value=colon<0?'':line.slice(colon+1); if(value.startsWith(' ')) value=value.slice(1);
    if(field==='data') { this.hasData=true; this.data.push(value); this.dataLength+=value.length; if(this.dataLength>2*1024*1024) { this.emit('Large SSE event','Preview limit reached; use the raw capture.'); this.reset(); } }
    if(field==='event') this.type=value||'message';
    if(field==='id'&&!value.includes('\0')) this.id=value;
  }
}

export class WebSocketDecoder {
  constructor(emit) { this.emit=emit; this.pending=new Uint8Array(); this.skip=0; }
  push(bytes) {
    if(this.skip) { const n=Math.min(this.skip,bytes.length); this.skip-=n; bytes=bytes.subarray(n); }
    if(!bytes.length) return;
    const data=new Uint8Array(this.pending.length+bytes.length); data.set(this.pending); data.set(bytes,this.pending.length);
    let pos=0;
    while(data.length-pos>=2) {
      const first=data[pos],second=data[pos+1],masked=!!(second&128); let len=second&127,header=2;
      if(len===126) { if(data.length-pos<4) break; len=data[pos+2]*256+data[pos+3]; header=4; }
      else if(len===127) { if(data.length-pos<10) break; let big=0n; for(let i=2;i<10;i++) big=(big<<8n)|BigInt(data[pos+i]); if(big>BigInt(Number.MAX_SAFE_INTEGER)) { this.emit('Invalid WebSocket frame','Length exceeds safe integer range. Inspect the raw capture.'); this.pending=new Uint8Array(); this.skip=Infinity; return; } len=Number(big); header=10; }
      if(masked) header+=4;
      if(data.length-pos<header) break;
      if(len>2*1024*1024) { this.emit('Large WebSocket frame',`${len.toLocaleString()} bytes; inspect raw pages or download the full frame.`); const available=data.length-pos-header; if(available<len) { this.skip=len-available; pos=data.length; break; } pos+=header+len; continue; }
      if(data.length-pos<header+len) break;
      const payload=data.slice(pos+header,pos+header+len);
      if(masked) for(let i=0;i<len;i++) payload[i]^=data[pos+header-4+(i%4)];
      const op=first&15; let compressed=!!(first&64); const names={0:'Continuation',1:'Text',2:'Binary',8:'Close',9:'Ping',10:'Pong'};
      if(op===1||op===2) { this.messageOpcode=op; this.messageCompressed=compressed; this.messageText=new TextDecoder(); }
      if(op===0) compressed=!!this.messageCompressed;
      const label=`WebSocket · ${names[op]||'Opcode '+op}${first&128?'':' · fragmented'}${compressed?' · compressed':''}`;
      const binary=compressed||op===2||op===8||(op===0&&this.messageOpcode===2);
      const text=(op===1||op===0)?(this.messageText??new TextDecoder()).decode(payload,{stream:!(first&128)}):new TextDecoder().decode(payload);
      this.emit(label,binary?hex(payload):text); pos+=header+len;
    }
    this.pending=data.slice(pos);
  }
}

export class CaptureDecoder {
  constructor(emit,onHeaders,onText) { this.emit=emit; this.onHeaders=onHeaders; this.onText=onText; this.pending=new Uint8Array(); this.mode='headers'; this.text=new TextDecoder(); }
  push(bytes) {
    if(this.mode==='headers') {
      const data=new Uint8Array(this.pending.length+bytes.length); data.set(this.pending); data.set(bytes,this.pending.length);
      let end=-1; for(let i=Math.max(0,this.pending.length-3);i<data.length-3;i++) if(data[i]===13&&data[i+1]===10&&data[i+2]===13&&data[i+3]===10) { end=i; break; }
      if(end<0) { if(data.length>256*1024) { this.mode='text'; this.onText(this.text.decode(data,{stream:true})); this.pending=new Uint8Array(); } else this.pending=data; return; }
      const headers=new TextDecoder().decode(data.subarray(0,end)); this.onHeaders(headers); bytes=data.subarray(end+4); this.pending=new Uint8Array();
      if(/^content-type:\s*text\/event-stream\b/im.test(headers)) { this.mode='sse'; this.decoder=new SSEDecoder(this.emit); }
      else if(/^upgrade:\s*websocket\s*$/im.test(headers)) { this.mode='websocket'; this.decoder=new WebSocketDecoder(this.emit); }
      else this.mode='text';
    }
    if(this.mode==='websocket') this.decoder.push(bytes);
    else { const text=this.text.decode(bytes,{stream:true}); if(this.mode==='sse') this.decoder.push(text); else this.onText(text); }
  }
  end() { const final=this.text.decode(); if(this.mode==='headers'&&this.pending.length) this.onText(new TextDecoder().decode(this.pending)); else if(this.mode==='sse') this.decoder.push(final); else if(this.mode==='text') this.onText(final); }
}
