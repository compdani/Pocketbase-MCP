"""Run against disposable seeded stock fixture; persist only redacted results."""
import subprocess,json,re,urllib.parse,hashlib,base64,pathlib
base='http://127.0.0.1:18093'
creds=json.loads(pathlib.Path('/tmp/mcp3stock/mcp-client.json').read_text())
transcript=[]
def curl(path,method='GET',data=None,headers=None,form=False):
 args=['curl','-sS','-i','-X',method,base+path]
 if headers:
  for k,v in headers.items():args+=['-H',k+': '+v]
 if data is not None:args+=['-H','Content-Type: '+('application/x-www-form-urlencoded' if form else 'application/json'),'--data',urllib.parse.urlencode(data) if form else json.dumps(data)]
 out=subprocess.check_output(args,text=True);head,body=out.split('\r\n\r\n' if '\r\n\r\n' in out else '\n\n',1)
 status=int(head.split()[1]);transcript.append(f'$ curl -i -X {method} {base+path.split("?")[0]} [sensitive arguments omitted]\nHTTP {status}\n')
 return status,head,body
s,h,b=curl('/.well-known/oauth-authorization-server');assert s==200
transcript.append(b)
for pkce in [False,True]:
 transcript.append('\nChatGPT hand-created confidential client; S256='+str(pkce))
 verifier='v'*50;challenge=base64.urlsafe_b64encode(hashlib.sha256(verifier.encode()).digest()).decode().rstrip('=')
 q=dict(response_type='code',client_id=creds['client_id'],redirect_uri=creds['redirect_uri'],state='curl-test')
 if pkce:q.update(code_challenge=challenge,code_challenge_method='S256')
 s,h,b=curl('/oauth/login?'+urllib.parse.urlencode(q));assert s==200
 cookie=re.search(r'(?im)^set-cookie: ([^;]+)',h)[1];csrf=re.search(r'name="csrf_token" value="([^"]+)"',b)[1]
 s,h,b=curl('/oauth/login','POST',dict(identity='user@example.com',password='password123',csrf_token=csrf),{'Cookie':cookie},True);assert s==302
 loc=re.search(r'(?im)^location: (.+)',h)[1].strip();code=urllib.parse.parse_qs(urllib.parse.urlparse(loc).query)['code'][0]
 form=dict(grant_type='authorization_code',client_id=creds['client_id'],client_secret=creds['client_secret'],code=code,redirect_uri=creds['redirect_uri'])
 if pkce:form['code_verifier']=verifier
 s,h,b=curl('/oauth/token','POST',form,form=True);assert s==200
 token=json.loads(b)['access_token'];transcript.append('PB bearer token returned (redacted).')
 s,h,b=curl('/mcp','POST',dict(jsonrpc='2.0',id=1,method='tools/list'),{'Authorization':'Bearer '+token});assert s==200 and 'search_items' in b
 transcript.append(b)
pathlib.Path('verification/stock-curl-transcript.txt').write_text('\n'.join(transcript)+'\nPASS: both flows\n')
print('PASS: curl baseline without PKCE and with S256; authorized tools/list')
