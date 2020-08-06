const protocol = '4'

const wsserver = (url, slot) => {
  const u = new URL(url)
  let protocol = 'wss:'
  if (u.protocol === 'http:') {
    protocol = 'ws:'
  }
  let path = u.pathname + slot
  if (!path.startsWith("/")) {
    path = "/" + path
  }
  return protocol + "//" + u.host + path
}

// newwormhole creates wormhole, the A side.
export const newwormhole = async (signal, pccb) => {
  const ws = new WebSocket(wsserver(signal, ''), protocol)
  let key, pass, pc
  let slotC, connC
  const slotP = new Promise((resolve, reject) => {
    slotC = { resolve, reject }
  })
  const connP = new Promise((resolve, reject) => {
    connC = { resolve, reject }
  })
  ws.onmessage = async m => {
    if (!pc) {
      const initmsg = JSON.parse(m.data)
      pass = crypto.getRandomValues(new Uint8Array(2))
      console.log('assigned slot:', initmsg.slot)
      // Initialise pc *before* passing the code back guarantees us that the B message
      // will not arrive while we're initialising pc.
      // This API is garbage.
      pc = await pccb(initmsg.iceServers)
      const slot = parseInt(initmsg.slot)
      if (isNaN(slot)) {
        slotC.reject('invalid slot')
        return
      }
      slotC.resolve(webwormhole.encode(slot, pass))
      return
    }
    if (!key) {
      console.log('got pake message a:', m.data)
      let msgB
      [key, msgB] = webwormhole.exchange(pass, m.data)
      console.log('message b:', msgB)
      if (key == null) {
        connC.reject('could not generate key')
        return
      }
      console.log('generated key')
      ws.send(msgB)
      pc.onicecandidate = e => {
        if (e.candidate && e.candidate.candidate !== '') {
          console.log('got local candidate')
          ws.send(webwormhole.seal(key, JSON.stringify(e.candidate)))
        } else if (!e.candidate) {
          logNAT(pc.localDescription.sdp)
        }
      }
      await pc.setLocalDescription(await pc.createOffer())
      console.log('created offer')
      ws.send(webwormhole.seal(key, JSON.stringify(pc.localDescription)))
      return
    }
    const jsonmsg = webwormhole.open(key, m.data)
    if (jsonmsg === null) {
      // Auth failed. Send something so B knows.
      ws.send(webwormhole.seal(key, 'bye'))
      ws.close()
      connC.reject('bad key')
      return
    }
    const msg = JSON.parse(jsonmsg)
    if (msg.type === 'answer') {
      await pc.setRemoteDescription(new RTCSessionDescription(msg))
      console.log('got answer')
      return
    }
    if (msg.candidate) {
      pc.addIceCandidate(new RTCIceCandidate(msg))
      console.log('got remote candidate')
      return
    }
    console.log('unknown message type', msg)
  }
  ws.onopen = e => {
    console.log('websocket session established')
  }
  ws.onerror = e => {
    connC.reject("couldn't connect to signalling server")
    console.log('websocket session error', e)
  }
  ws.onclose = e => {
    // TODO hardcoded codes here for now. At somepoint, dialling code should
    // be in the wasm portion and reuse server symbols.
    if (e.code === 4000) {
      connC.reject('no such slot')
    } else if (e.code === 4001) {
      connC.reject('timed out')
    } else if (e.code === 4002) {
      connC.reject('could not get slot')
    } else if (e.code === 4003) {
      connC.reject('wrong protocol version, must update')
    } else if (e.code === 4004 || e.code === 1001) {
      // Workaround for regression introduced in firefox around version ~78.
      // Usually the websocket connection stays open for the duration of the session, since
      // it doesn't hurt and it make candidate trickling easier. We only do this here out of
      // laziness. The go code has more disciplined websocket lifecycle management.
      // Recent versions of Firefox introduced a bug where websocket connections are killed
      // when a download begins. This would happen after the WebRTC connection is set up
      // so it's not really an error we need to react to.
      connC.resolve()
    } else {
      connC.reject(`websocket session closed: ${e.reason} (${e.code})`)
    }
  }

  return [await slotP, connP]
}

// dial joins a wormhole, the B side.
export const dial = async (signal, code, pccb) => {
  let key, pc
  let connC
  const connP = new Promise((resolve, reject) => {
    connC = { resolve, reject }
  })
  const [slot, pass] = webwormhole.decode(code)
  if (pass.length === 0) {
    throw 'bad code'
  }

  console.log('dialling slot:', slot)
  const ws = new WebSocket(wsserver(signal, slot), protocol)
  ws.onmessage = async m => {
    if (!pc) {
      const initmsg = JSON.parse(m.data)
      pc = await pccb(initmsg.iceServers)
      return
    }
    if (!key) {
      console.log('got pake message b:', m.data)
      key = webwormhole.finish(m.data)
      if (key == null) {
        connC.reject('could not generate key')
        return
      }
      console.log('generated key')
      pc.onicecandidate = e => {
        if (e.candidate && e.candidate.candidate !== '') {
          console.log('got local candidate')
          ws.send(webwormhole.seal(key, JSON.stringify(e.candidate)))
        } else if (!e.candidate) {
          logNAT(pc.localDescription.sdp)
        }
      }
      return
    }
    const jmsg = webwormhole.open(key, m.data)
    if (jmsg == null) {
      // Auth failed. Send something so A knows.
      ws.send(webwormhole.seal(key, 'bye'))
      ws.close()
      connC.reject('bad key')
      return
    }
    const msg = JSON.parse(jmsg)
    if (msg.type === 'offer') {
      await pc.setRemoteDescription(new RTCSessionDescription(msg))
      console.log('got offer')
      await pc.setLocalDescription(await pc.createAnswer())
      console.log('created answer')
      ws.send(webwormhole.seal(key, JSON.stringify(pc.localDescription)))
      return
    }
    if (msg.candidate) {
      pc.addIceCandidate(new RTCIceCandidate(msg))
      console.log('got remote candidate')
      return
    }
    console.log('unknown message type', msg)
  }
  ws.onopen = async e => {
    console.log('websocket opened')
    const msgA = webwormhole.start(pass)
    if (msgA == null) {
      connC.reject("couldn't generate A's PAKE message")
      return
    }
    console.log('message a:', msgA)
    ws.send(msgA)
  }
  ws.onerror = e => {
    connC.reject("couldn't connect to signalling server")
    console.log('websocket session error', e)
  }
  ws.onclose = e => {
    // TODO hardcoded codes here for now. At somepoint, dialling code should
    // be in the wasm portion and reuse server symbols.
    if (e.code === 4000) {
      connC.reject('no such slot')
    } else if (e.code === 4001) {
      connC.reject('timed out')
    } else if (e.code === 4002) {
      connC.reject('could not get slot')
    } else if (e.code === 4003) {
      connC.reject('wrong protocol version, must update')
    } else if (e.code === 4004 || e.code === 1001) {
      // Workaround for regression introduced in firefox around version ~78.
      // Usually the websocket connection stays open for the duration of the session, since
      // it doesn't hurt and it make candidate trickling easier. We only do this here out of
      // laziness. The go code has more disciplined websocket lifecycle management.
      // Recent versions of Firefox introduced a bug where websocket connections are killed
      // when a download begins. This would happen after the WebRTC connection is set up
      // so it's not really an error we need to react to.
      connC.resolve()
    } else {
      connC.reject(`websocket session closed: ${e.reason} (${e.code})`)
    }
  }
  return connP
}

// logNAT tries to guess the type of NAT based on candidates and log it.
const logNAT = sdp => {
  let count = 0; let host = 0; let srflx = 0
  const portmap = new Map()

  const lines = sdp.replace(/\r/g, '').split('\n')
  for (let i = 0; i < lines.length; i++) {
    if (!lines[i].startsWith('a=candidate:')) {
      continue
    }
    const parts = lines[i].substring('a=candidate:'.length).split(' ')
    const proto = parts[2].toLowerCase()
    const port = parts[5]
    const typ = parts[7]
    if (proto !== 'udp') {
      continue
    }
    count++
    if (typ === 'host') {
      host++
    } else if (typ === 'srflx') {
      srflx++
      let rport = ''
      for (let j = 8; j < parts.length; j += 2) {
        if (parts[j] === 'rport') {
          rport = parts[j + 1]
        }
      }
      if (!portmap.get(rport)) {
        portmap.set(rport, new Set())
      }
      portmap.get(rport).add(port)
    }
  }
  console.log(`local udp candidates: ${count} (host: ${host} stun: ${srflx})`)
  let maxmapping = 0
  portmap.forEach(v => {
    if (v.size > maxmapping) {
      maxmapping = v.size
    }
  })
  if (maxmapping === 0) {
    console.log('nat: unknown: ice disabled or stun blocked')
  } else if (maxmapping === 1) {
    console.log('nat: cone or none: 1:1 port mapping')
  } else if (maxmapping > 1) {
    console.log('nat: symmetric: 1:n port mapping (bad news)')
  } else {
    console.log('nat: failed to estimate nat type')
  }
  console.log('for more webrtc troubleshooting try https://test.webrtc.org/ and your browser webrtc logs (about:webrtc or chrome://webrtc-internals/)')
}
