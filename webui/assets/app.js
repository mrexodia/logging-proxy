import {CaptureDecoder,fromBase64,hex,pretty} from './decoders.mjs';
const $=id=>document.getElementById(id);
const state={mode:'requests',offset:0,total:0,paused:false,revision:0,key:null,detail:null,direction:'response',tail:null,tailName:null,tailOffset:0,following:true,rawOffset:0,rawSize:0,listSeq:0,detailSeq:0,rawSeq:0};
const bytes=n=>n<1024?`${n} B`:n<1048576?`${(n/1024).toFixed(1)} KiB`:`${(n/1048576).toFixed(1)} MiB`;
const time=value=>new Date(value).toLocaleString(undefined,{month:'short',day:'numeric',hour:'2-digit',minute:'2-digit',second:'2-digit'});
function notice(id,text) { $(id).textContent=text||''; $(id).hidden=!text; }
async function api(path) { const response=await fetch(path); if(!response.ok) throw new Error(`Viewer returned ${response.status}: ${await response.text()}`); return response.json(); }
function node(tag,text,className) { const el=document.createElement(tag); if(text!==undefined) el.textContent=text; if(className) el.className=className; return el; }
function fileURL(name,download=false) { return `/api/file?name=${encodeURIComponent(name)}${download?'&download=1':''}`; }
function currentFile() { return state.detail?.[state.direction+'_file']; }

function syncDays(days) {
  const select=$('day'), selected=select.value;
  const values=[...new Set([...(days||[]),...(selected?[selected]:[])])].sort().reverse();
  const options=['',...values];
  if(JSON.stringify(Array.from(select.options,option=>option.value))!==JSON.stringify(options)) {
    select.replaceChildren(...options.map(day=>{const option=node('option',day||'All days');option.value=day;return option;}));
    select.value=selected;
  }
}

async function refresh() {
  const seq=++state.listSeq; const params=new URLSearchParams({offset:state.offset,limit:100,q:$('search').value,day:$('day').value});
  if(state.mode==='requests') params.set('state',$('state').value);
  try {
    const result=await api(`/api/${state.mode==='requests'?'requests':'files'}?${params}`); if(seq!==state.listSeq) return;
    if(state.offset>=result.total&&state.offset>0) { state.offset=Math.max(0,Math.floor((result.total-1)/100)*100); return refresh(); }
    state.total=result.total; syncDays(result.days); notice('problem',result.problem);
    if(state.mode==='requests') { $('total').textContent=result.all.toLocaleString(); $('unfinished').textContent=result.unfinished.toLocaleString(); $('errors').textContent=result.errors.toLocaleString(); }
    $('rows').replaceChildren(); $('table-head').replaceChildren(); const heading=node('tr');
    for(const title of state.mode==='requests'?['Time / method','Destination','Status / size']:['Size','Capture file','Open']) heading.append(node('th',title)); $('table-head').append(heading);
    for(const item of result.items) {
      const row=node('tr');
      if(state.mode==='files') {
        row.append(node('td',bytes(item.size),'muted')); const cell=node('td'); const label=node('div',item.name); label.style.overflowWrap='anywhere'; cell.append(label,node('div',item.problem||time(item.modified),'muted')); row.append(cell); const action=node('td'); const a=node('a','Download'); a.href=fileURL(item.name,true); a.download=item.name.split('/').pop(); action.append(a); row.append(action);
      } else {
        if(item.key===state.key) row.classList.add('current'); const meta=item.metadata; const left=node('td'); left.append(node('div',meta.method||'—','method'),node('div',time(item.started),'time')); row.append(left);
        const middle=node('td'); const button=node('button',undefined,'row-link'); let target=meta.target_url||meta.source_url||item.metadata_files?.[0]||'Unknown request'; let path=target,host=''; try { const url=new URL(target); path=url.pathname+url.search; host=url.host; } catch {}
        button.title=target; button.append(node('strong',path),node('span',host||meta.id||'Metadata unavailable')); button.addEventListener('click',()=>select(item.key)); middle.append(button); row.append(middle);
        const right=node('td'); right.append(node('div',meta.response_status_code||'—',`status ${meta.response_status_code>=400||item.state==='error'?'error':item.state==='unfinished'?'pending':''}`),node('div',item.state==='unfinished'?'unfinished':bytes(item.response_bytes),'time')); row.append(right);
      }
      $('rows').append(row);
    }
    $('empty').hidden=result.total!==0; $('range').textContent=result.total?`${state.offset+1}–${Math.min(state.offset+100,result.total)} of ${result.total.toLocaleString()}`:'0 captures'; $('previous').disabled=state.offset===0; $('next').disabled=state.offset+100>=result.total;
  } catch(error) { if(seq===state.listSeq) notice('problem',error.message); }
}

async function select(key,update=false) {
  const seq=++state.detailSeq;
  if(!update) { closeTail(); state.key=key; state.detail=null; state.rawOffset=0; state.tailName=null; state.following=true; $('follow').textContent='Pause stream'; $('raw-content').textContent=''; $('messages').replaceChildren(); $('select-hint').hidden=true; $('detail').hidden=false; $('detail-title').textContent='Loading capture…'; }
  try {
    const detail=await api(`/api/requests/${key}`); if(seq!==state.detailSeq||key!==state.key) return;
    state.detail=detail; const meta=detail.metadata;
    $('detail-title').textContent=`${meta.method||'Request'} · ${meta.response_status_code||'Awaiting response'} · ${detail.state}`;
    $('detail-url').textContent=meta.target_url||meta.source_url||'';
    $('detail-meta').textContent=`${time(detail.started)} · ${detail.duration_ms} ms recorded · ${meta.id||'No request ID'}`;
    $('metadata').textContent=JSON.stringify(detail,null,2); $('metadata-links').replaceChildren();
    for(const name of detail.metadata_files||[]) { const a=node('a',name); a.href=fileURL(name,true); a.download=name.split('/').pop(); $('metadata-links').append(a); }
    notice('detail-problem',(detail.problems||[]).join('\n'));
    syncCapture(); if(!update) refresh();
  } catch(error) { if(seq===state.detailSeq) notice('detail-problem',error.message); }
}

function closeTail() { state.tail?.close(); state.tail=null; }
let decoder; let renderTimer; let messageBytes=0;
let textPreview='';
function resetPreview() {
  textPreview=''; messageBytes=0; $('raw-content').textContent=''; $('raw-content').classList.remove('stream-headers'); $('messages').replaceChildren(); state.tailOffset=0;
  decoder=new CaptureDecoder((label,text)=>{
    const preview=pretty(text).slice(0,2*1024*1024); const card=node('article',undefined,'message'); card.dataset.bytes=String(preview.length); messageBytes+=preview.length; card.append(node('div',label,'message-label'),node('pre',preview)); $('messages').append(card); while($('messages').childElementCount>300||messageBytes>2*1024*1024) {messageBytes-=Number($('messages').firstChild.dataset.bytes);$('messages').firstChild.remove();} scrollBottom();
  },headers=>{ appendText(headers+'\n\n'); },appendText);
}
function appendText(text) { textPreview=(textPreview+text).slice(-2*1024*1024); if(!renderTimer) renderTimer=setTimeout(()=>{ renderTimer=null; if($('view').value==='stream') { $('raw-content').textContent=textPreview; scrollBottom(); } },80); }
function scrollBottom() { const el=$('stream-content'); if(el.scrollHeight-el.scrollTop-el.clientHeight<300) el.scrollTop=el.scrollHeight; }
function syncCapture() {
  const name=currentFile(); $('download').hidden=!name; if(name) { $('download').href=fileURL(name,true); $('download').download=name.split('/').pop(); }
  if(!name) { closeTail(); state.tailName=null; resetPreview(); $('stream-status').textContent='Waiting for capture file…'; return; }
  if($('view').value!=='stream') { closeTail(); loadRaw(); return; }
  if(state.tailName!==name) { closeTail(); state.tailName=name; resetPreview(); if(state.following)startTail();else $('stream-status').textContent='Stream paused'; }
  else if(!state.tail&&state.following&&$('stream-status').textContent==='Waiting for capture file…') startTail();
}
function startTail() {
  const name=currentFile(); if(!name||!state.following||$('view').value!=='stream') return;
  closeTail(); const stream=new EventSource(`/api/tail?name=${encodeURIComponent(name)}&offset=${state.tailOffset}`); state.tail=stream;
  $('stream-status').textContent='Connecting to capture…';
  stream.onopen=()=>{ if(state.tail===stream) $('stream-status').textContent=`Following · ${bytes(state.tailOffset)}`; };
  stream.addEventListener('chunk',event=>{
    if(state.tail!==stream) return; const chunk=JSON.parse(event.data);
    if(chunk.offset!==state.tailOffset) { notice('detail-problem','Stream offset mismatch. Reopen this capture to replay it.'); closeTail(); return; }
    decoder.push(fromBase64(chunk.data)); state.tailOffset=chunk.next; $('stream-status').textContent=`Following · ${bytes(state.tailOffset)}`;
  });
  stream.addEventListener('end',()=>{ if(state.tail!==stream) return; decoder.end(); $('stream-status').textContent=`Complete · ${bytes(state.tailOffset)}`; closeTail(); });
  stream.addEventListener('waiting',()=>{ if(state.tail===stream) $('stream-status').textContent=`Waiting for more bytes · ${bytes(state.tailOffset)}`; });
  stream.addEventListener('failure',()=>{ if(state.tail!==stream) return; $('stream-status').textContent='Capture read failed'; closeTail(); });
  stream.onerror=()=>{ if(state.tail===stream) $('stream-status').textContent='Reconnecting · captured bytes retained'; };
}
async function loadRaw() {
  const name=currentFile(); if(!name) return; const seq=++state.rawSeq;
  try { const result=await api(`${fileURL(name)}&offset=${state.rawOffset}`); if(seq!==state.rawSeq||name!==currentFile()||$('view').value==='stream') return;
    const data=fromBase64(result.data); state.rawSize=result.size;
    $('raw-content').textContent=$('view').value==='hex'?Array.from({length:Math.ceil(data.length/16)},(_,i)=>`${(state.rawOffset+i*16).toString(16).padStart(8,'0')}  ${hex(data.subarray(i*16,i*16+16))}`).join('\n'):new TextDecoder().decode(data);
    $('messages').replaceChildren(); $('raw-range').textContent=`${result.offset.toLocaleString()}–${result.next.toLocaleString()} / ${bytes(result.size)}`; $('raw-previous').disabled=state.rawOffset===0; $('raw-next').disabled=result.next>=result.size; $('stream-status').textContent='Original bytes · paginated';
  } catch(error) { if(seq===state.rawSeq) notice('detail-problem',error.message); }
}

for(const mode of ['requests','files']) $(mode+'-tab').onclick=()=>{ state.mode=mode; state.offset=0; for(const value of ['requests','files']) { $(value+'-tab').classList.toggle('selected',value===mode); $(value+'-tab').setAttribute('aria-pressed',String(value===mode)); } $('state').disabled=mode==='files'; $('stats').hidden=mode==='files'; $('search').placeholder=mode==='files'?'Search filenames, including archives…':'Search URL, ID, method, status…'; refresh(); };
let searchTimer; $('search').oninput=()=>{ clearTimeout(searchTimer); searchTimer=setTimeout(()=>{state.offset=0;refresh();},200); }; $('state').onchange=()=>{state.offset=0;refresh();};
$('day').onchange=()=>{state.offset=0;refresh();};
$('previous').onclick=()=>{state.offset=Math.max(0,state.offset-100);refresh();}; $('next').onclick=()=>{state.offset+=100;refresh();}; $('refresh').onclick=()=>{refresh();if(state.key) select(state.key,true);};
$('pause').onclick=()=>{state.paused=!state.paused;$('pause').textContent=state.paused?'Resume updates':'Pause updates';$('connection').textContent=state.paused?'Updates paused':'Live';if(!state.paused){refresh();if(state.key)select(state.key,true);}};
for(const direction of ['request','response']) $(direction+'-direction').onclick=()=>{ state.direction=direction; state.rawOffset=0; for(const value of ['request','response']) { $(value+'-direction').classList.toggle('selected',value===direction); $(value+'-direction').setAttribute('aria-pressed',String(value===direction)); } syncCapture(); };
$('view').onchange=()=>{ const raw=$('view').value!=='stream'; $('raw-controls').hidden=!raw; $('follow').hidden=raw; $('preview-note').hidden=raw; state.tailName=null; state.rawOffset=0; state.rawSeq++; syncCapture(); };
$('follow').onclick=()=>{ state.following=!state.following; $('follow').textContent=state.following?'Pause stream':'Resume stream'; if(state.following)startTail();else{closeTail();$('stream-status').textContent=`Paused at ${bytes(state.tailOffset)} · capture continues on disk`;}};
$('raw-previous').onclick=()=>{state.rawOffset=Math.max(0,state.rawOffset-65536);loadRaw();}; $('raw-next').onclick=()=>{state.rawOffset+=65536;loadRaw();};
const events=new EventSource('/api/events');
events.onopen=()=>{ $('connection-dot').classList.add('live'); $('connection').textContent=state.paused?'Updates paused':'Live'; };
events.onerror=()=>{ $('connection-dot').classList.remove('live'); $('connection').textContent='Reconnecting…'; };
events.addEventListener('change',event=>{ const data=JSON.parse(event.data); if(state.paused)return; notice('problem',data.problem); if(state.revision!==data.revision){state.revision=data.revision;refresh();if(state.key)select(state.key,true);} });
window.addEventListener('pagehide',()=>{events.close();closeTail();});
refresh();
