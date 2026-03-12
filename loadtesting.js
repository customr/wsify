// test-with-cleanup.js
import { check, sleep } from 'k6';
import ws from 'k6/ws';
import http from 'k6/http';

export let options = {
    stages: [
        { duration: '30s', target: 50 },   // Ramp up to 50 users
        { duration: '1m', target: 100 },    // Stay at 100 users
        { duration: '30s', target: 200 },   // Ramp up to 200
        { duration: '1m', target: 200 },    // Stay at 200
        { duration: '30s', target: 0 },      // Ramp down
    ]
};

export default function() {
    const vu = __VU;
    const wsUrl = 'ws://localhost:3000/ws';
    const broadcastUrl = 'http://localhost:3000/broadcast?key=test';
    
    let messagesReceived = 0;
    
    const res = ws.connect(wsUrl, {}, function(socket) {
        socket.on('open', () => {
            // Join channel
            socket.send(JSON.stringify({
                command: 'join',
                args: { channel: 'test' }
            }));
            
            // Send broadcast
            setTimeout(() => {
                http.post(broadcastUrl,
                    JSON.stringify({
                        channel: 'test',
                        content: { msg: 'hello' }
                    }),
                    { headers: { 'Content-Type': 'application/json' } }
                );
            }, 1000);
        });
        
        socket.on('message', () => {
            messagesReceived++;
        });
        
        socket.on('close', () => {
            // Always leave channel on close
            socket.send(JSON.stringify({
                command: 'leave',
                args: { channel: 'test' }
            }));
        });
        
        // Auto-disconnect after 5 seconds
        setTimeout(() => {
            socket.close();
        }, 5000);
    });
    
    check(res, {
        'connected': (r) => r && r.status === 101,
        'received messages': () => messagesReceived > 0
    });
    
    sleep(1);
}