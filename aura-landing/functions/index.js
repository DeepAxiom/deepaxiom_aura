const functions = require("firebase-functions");
const admin = require("firebase-admin");

admin.initializeApp();
const db = admin.firestore();


exports.addToWaitlist = functions.https.onRequest(async (req, res) => {
    res.set('Access-Control-Allow-Origin', '*');

    if (req.method === 'OPTIONS') {
        res.set('Access-Control-Allow-Methods', 'POST');
        res.set('Access-Control-Allow-Headers', 'Content-Type');
        res.set('Access-Control-Max-Age', '3600');
        res.status(204).send('');
        return;
    }
    
    if (req.method !== "POST") {
        res.status(405).send("Method Not Allowed");
        return;
    }

    try {
        const data = req.body;

        const serverMetadata = {
            timestamp: admin.firestore.FieldValue.serverTimestamp(),
            ip: req.headers['x-forwarded-for'] || req.socket.remoteAddress || 'N/A',
            userAgent: req.headers['user-agent'] || 'N/A',
        };
        
        const userDocument = {
            email: data.email,
            whatsapp: data.whatsapp || null,
            interests: data.interests || [],
            clientMetadata: data.clientMetadata || {},
            serverMetadata: serverMetadata
        };

        await db.collection("website_users").add(userDocument);

        res.status(200).json({ message: "Successfully added to the waitlist!" });

    } catch (error) {
        console.error("Error adding user to waitlist:", error);
        res.status(500).json({ error: "Failed to add user to the waitlist." });
    }
});


exports.submitProjectRequest = functions.https.onRequest(async (req, res) => {
    // Headers CORS
    res.set('Access-Control-Allow-Origin', '*');
    
    if (req.method === 'OPTIONS') {
        res.set('Access-Control-Allow-Methods', 'POST');
        res.set('Access-Control-Allow-Headers', 'Content-Type');
        res.status(204).send('');
        return;
    }

    if (req.method !== "POST") {
        res.status(405).send("Method Not Allowed");
        return;
    }

    try {
        const data = req.body;
        
        const leadDocument = {
            target: data.target || "N/A", 
            industry: data.industry || "N/A",
            timeline: {
                start: data.startDate || null,
                end: data.endDate || null,
                urgent: Boolean(data.urgent) 
            },
            budget: data.budget || "N/A",
            description: data.description || "",
            contact: {
                name: data.name,
                email: data.email,
                phone: data.phone || null
            },
            meta: {
                submittedAt: admin.firestore.FieldValue.serverTimestamp(),
                lang: data.meta && data.meta.lang ? data.meta.lang : 'es',
                ip: req.headers['x-forwarded-for'] || req.socket.remoteAddress || 'N/A',
                userAgent: req.headers['user-agent'] || 'N/A'
            },
            status: "NEW" 
        };

        await db.collection("sales_leads").add(leadDocument);

        res.status(200).json({ message: "Project request received successfully" });

    } catch (error) {
        console.error("Error submitting project request:", error);
        res.status(500).json({ error: "Internal Server Error" });
    }
});