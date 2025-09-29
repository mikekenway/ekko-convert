import express from 'express';
import { createServer } from 'http';
import { Server as SocketServer } from 'socket.io';
import multer from 'multer';
import ffmpeg from 'fluent-ffmpeg';
import cors from 'cors';
import { join } from 'path';
import { randomUUID } from 'crypto';
import { writeFile, unlink, mkdir } from 'fs/promises';

const app = express();
const server = createServer(app);
const io = new SocketServer(server, {
    cors: {
        origin: "http://localhost:3000",
        methods: ["GET", "POST"]
    }
});
const PORT = process.env.PORT || 3001;

// Track active FFmpeg processes for cancellation
const activeConversions = new Map<string, any>();

// Middleware
app.use(cors());
app.use(express.json({ limit: '15gb' }));
app.use(express.urlencoded({ extended: true, limit: '15gb' }));

// Error handling middleware for multer
app.use((error, req, res, next) => {
    if (error instanceof multer.MulterError) {
        if (error.code === 'LIMIT_FILE_SIZE') {
            return res.status(400).json({ 
                error: `File too large. Maximum size allowed is 15GB.` 
            });
        }
        if (error.code === 'LIMIT_FILE_COUNT') {
            return res.status(400).json({ 
                error: 'Too many files. Please upload one file at a time.' 
            });
        }
        return res.status(400).json({ 
            error: `Upload error: ${error.message}` 
        });
    }
    next(error);
});

// Configure multer for file uploads
const upload = multer({
    storage: multer.memoryStorage(),
    limits: { fileSize: 15 * 1024 * 1024 * 1024 } // 15GB limit
});

// Create downloads directory
const downloadsDir = join(process.cwd(), 'public', 'downloads');
await mkdir(downloadsDir, { recursive: true });

// Serve converted files
app.use('/downloads', express.static(downloadsDir));

// Health check endpoint
app.get('/api/health', (req, res) => {
    res.json({ status: 'ok', service: 'video-converter' });
});

// Get video metadata
app.post('/api/analyze', upload.single('file'), async (req, res) => {
    try {
        const { file } = req;

        if (!file) {
            return res.status(400).json({ error: 'No file uploaded' });
        }

        // Write file temporarily for analysis
        const tempPath = join(
            downloadsDir,
            `temp-${randomUUID()}-${file.originalname}`
        );
        await writeFile(tempPath, file.buffer);

        // Get metadata using FFmpeg
        const metadata = await new Promise((resolve, reject) => {
            ffmpeg.ffprobe(tempPath, (err, metadata) => {
                if (err) reject(err);
                resolve(metadata);
            });
        });

        // Clean up temp file
        await unlink(tempPath);

        res.json({
            name: file.originalname,
            duration: metadata.format.duration,
            fileSize: metadata.format.size,
            resolution: `${metadata.streams[0].width}x${metadata.streams[0].height}`,
            codec: metadata.streams[0].codec_name,
            bitrate: metadata.format.bit_rate
        });
    } catch (error) {
        console.error('Analysis error:', error);
        res.status(500).json({ error: 'Analysis failed' });
    }
});

// Video conversion endpoint
app.post('/api/convert', upload.single('file'), async (req, res) => {
    try {
        const { file } = req;
        const { format } = req.body;

        if (!file) {
            return res.status(400).json({ error: 'No file uploaded' });
        }

        console.log(`Converting ${file.originalname} to ${format}`);

        // Generate unique filenames
        const inputName = `${randomUUID()}-${file.originalname}`;
        const outputName = `${randomUUID()}.${format}`;
        const inputPath = join(downloadsDir, inputName);
        const outputPath = join(downloadsDir, outputName);

        // Write uploaded file
        await writeFile(inputPath, file.buffer);

        // First, get the duration of the input file for progress calculation
        const inputDuration = await new Promise<number>((resolve, reject) => {
            ffmpeg.ffprobe(inputPath, (err, metadata) => {
                if (err) reject(err);
                else resolve(metadata.format.duration || 0);
            });
        });

        console.log(`Starting conversion of ${file.originalname} (${inputDuration}s) to ${format}`);
        
        // Emit initial progress event
        io.emit('conversion-progress', {
            fileName: file.originalname,
            progress: 0,
            outputName: outputName
        });
        
        // Convert with FFmpeg and track progress
        await new Promise((resolve, reject) => {
            let lastReportedProgress = 0;
            
            const ffmpegCommand = ffmpeg(inputPath);
            
            // Store the process for potential cancellation
            activeConversions.set(file.originalname, ffmpegCommand);
            
            ffmpegCommand
                .toFormat(format)
                .on('progress', (progress) => {
                    if (progress.timemark && inputDuration > 0) {
                        // Parse timemark (format: "00:01:23.45")
                        const timeParts = progress.timemark.split(':');
                        const currentSeconds = 
                            parseInt(timeParts[0]) * 3600 + // hours
                            parseInt(timeParts[1]) * 60 +    // minutes
                            parseFloat(timeParts[2]);        // seconds
                        
                        const percentComplete = Math.min(Math.round((currentSeconds / inputDuration) * 100), 100);
                        
                        // Only log and emit every 5% to avoid spam
                        if (percentComplete >= lastReportedProgress + 5 || percentComplete === 100) {
                            console.log(`Progress: ${file.originalname} - ${percentComplete}%`);
                            
                            // Emit progress to all connected clients
                            io.emit('conversion-progress', {
                                fileName: file.originalname,
                                progress: percentComplete,
                                outputName: outputName
                            });
                            
                            lastReportedProgress = percentComplete;
                        }
                    }
                })
                .on('end', () => {
                    console.log(`Conversion completed: ${outputName}`);
                    activeConversions.delete(file.originalname);
                    io.emit('conversion-progress', {
                        fileName: file.originalname,
                        progress: 100,
                        outputName: outputName,
                        completed: true
                    });
                    resolve(undefined);
                })
                .on('error', err => {
                    console.error('Conversion error:', err);
                    activeConversions.delete(file.originalname);
                    io.emit('conversion-error', {
                        fileName: file.originalname,
                        error: err.message
                    });
                    reject(err);
                })
                .save(outputPath);
        });

        // Clean up input file
        await unlink(inputPath);

        // Get metadata of converted file
        const metadata = await new Promise((resolve, reject) => {
            ffmpeg.ffprobe(outputPath, (err, metadata) => {
                if (err) reject(err);
                resolve(metadata);
            });
        });

        // Return download URL and metadata
        const downloadUrl = `/downloads/${outputName}`;
        res.json({
            name: file.originalname,
            outputName: outputName,
            downloadUrl: downloadUrl,
            duration: metadata.format.duration,
            fileSize: metadata.format.size,
            resolution: `${metadata.streams[0].width}x${metadata.streams[0].height}`,
            codec: metadata.streams[0].codec_name
        });
    } catch (error) {
        console.error('Server error:', error);
        
        let errorMessage = 'Conversion failed';
        let statusCode = 500;
        
        if (error.message) {
            if (error.message.includes('ENOENT')) {
                errorMessage = 'FFmpeg not found. Please ensure FFmpeg is installed.';
            } else if (error.message.includes('Unknown encoder')) {
                errorMessage = `Unsupported output format: ${format}`;
                statusCode = 400;
            } else if (error.message.includes('Invalid data found')) {
                errorMessage = 'Invalid or corrupted video file';
                statusCode = 400;
            } else if (error.message.includes('No space left')) {
                errorMessage = 'Server storage full. Please try again later.';
            } else {
                errorMessage = error.message;
            }
        }
        
        res.status(statusCode).json({ error: errorMessage });
    }
});

// Get list of converted files
app.get('/api/files', async (req, res) => {
    try {
        const fs = await import('fs/promises');
        const files = await fs.readdir(downloadsDir);
        const fileList = files.map(filename => ({
            name: filename,
            downloadUrl: `/downloads/${filename}`,
            createdAt: new Date().toISOString() // You could get actual file stats here
        }));

        res.json(fileList);
    } catch (error) {
        console.error('Error listing files:', error);
        res.status(500).json({ error: 'Failed to list files' });
    }
});

server.listen(PORT, () => {
    console.log(`Video converter server running on port ${PORT}`);
    console.log(`Health check: http://localhost:${PORT}/api/health`);
    console.log(`Downloads: http://localhost:${PORT}/downloads/`);
    console.log(`Socket.IO enabled for real-time progress updates`);
});

io.on('connection', (socket) => {
    console.log('Client connected for progress updates');
    
    socket.on('cancel-conversion', (data: { fileName: string }) => {
        console.log(`Cancellation requested for: ${data.fileName}`);
        
        const ffmpegCommand = activeConversions.get(data.fileName);
        if (ffmpegCommand) {
            console.log(`Cancelling conversion for: ${data.fileName}`);
            ffmpegCommand.kill('SIGKILL');
            activeConversions.delete(data.fileName);
            
            // Emit cancellation confirmation
            io.emit('conversion-error', {
                fileName: data.fileName,
                error: 'Conversion cancelled by user'
            });
        } else {
            console.log(`No active conversion found for: ${data.fileName}`);
        }
    });
    
    socket.on('disconnect', () => {
        console.log('Client disconnected');
    });
});
