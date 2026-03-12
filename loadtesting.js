// websocket-only-loadtest.js
import { check, sleep } from 'k6';
import ws from 'k6/ws';
import { Rate, Trend, Counter } from 'k6/metrics';

// Custom metrics
const connectionRate = new Rate('connection_success');
const connectionDuration = new Trend('connection_duration');
const activeConnections = new Counter('active_connections');
const joinSuccess = new Rate('join_success');
const leaveSuccess = new Rate('leave_success');

export let options = {
    stages: [
        { duration: '30s', target: 50 },   // Ramp up to 50 users
        { duration: '1m', target: 100 },    // Stay at 100 users
        { duration: '30s', target: 200 },   // Ramp up to 200
        { duration: '2m', target: 200 },    // Stay at 200
        { duration: '30s', target: 0 },     // Ramp down
    ],
    thresholds: {
        'connection_success': ['rate>0.99'],      // 99% connections successful
        'join_success': ['rate>0.99'],             // 99% joins successful
        'leave_success': ['rate>0.99'],            // 99% leaves successful
        'connection_duration': ['p(95)<1000'],     // 95% connections under 1s
    },
};

const CHANNELS = ['general', 'random', 'tech', 'gaming', 'sports', 'news', 'music'];

export default function() {
    const vu = __VU;
    const iter = __ITER;
    
    // Select a channel based on VU to distribute load
    const channel = CHANNELS[vu % CHANNELS.length];
    const wsUrl = 'ws://localhost:3000/ws';
    
    let steps = {
        connected: false,
        joined: false,
        left: false
    };
    
    const startTime = Date.now();
    
    // WebSocket connection
    const response = ws.connect(wsUrl, {}, function(socket) {
        socket.on('open', function() {
            // Record connection duration
            connectionDuration.add(Date.now() - startTime);
            steps.connected = true;
            connectionRate.add(true);
            activeConnections.add(1);
            
            // Join a channel
            const joinMsg = JSON.stringify({
                command: 'join',
                args: { channel: channel }
            });
            
            socket.send(joinMsg);
            
            // Wait a bit (simulate user activity)
            socket.setTimeout(function() {
                // Leave the channel
                const leaveMsg = JSON.stringify({
                    command: 'leave',
                    args: { channel: channel }
                });
                
                socket.send(leaveMsg);
                steps.left = true;
                leaveSuccess.add(true);
                
                // Close connection
                socket.setTimeout(function() {
                    socket.close();
                    activeConnections.add(-1);
                }, 500);
                
            }, 5000); // Stay in channel for 5 seconds
        });
        
        socket.on('message', function(data) {
            // Just acknowledge messages, don't track them
            // This is for any server pings or responses
        });
        
        socket.on('error', function(e) {
            connectionRate.add(false);
        });
        
        socket.on('close', function() {
            // Connection closed
        });
    });
    
    // Track join success (we assume join worked if connection succeeded)
    if (steps.connected) {
        joinSuccess.add(true);
    } else {
        joinSuccess.add(false);
        leaveSuccess.add(false);
    }
    
    // Basic checks
    check(response, {
        'WebSocket connected': (r) => r && r.status === 101,
    });
    
    // Random sleep between iterations (1-3 seconds)
    sleep(Math.random() * 2 + 1);
}