import React, { useState, useEffect } from 'react';
import { useMutation } from '@tanstack/react-query';
import { toast } from 'sonner';
import { io, Socket } from 'socket.io-client';
import { X } from 'lucide-react';
import { DropZone } from './DropZone';
import { Button } from './ui/button';
import { Progress } from './ui/progress';
import {
    Select,
    SelectTrigger,
    SelectValue,
    SelectContent,
    SelectItem
} from './ui/select';

interface ConvertedFile {
    name: string;
    outputName: string;
    downloadUrl: string;
    duration: number;
    fileSize: number;
    resolution: string;
    codec: string;
}

interface ConversionResponse {
    name: string;
    outputName: string;
    downloadUrl: string;
    duration: number;
    fileSize: number;
    resolution: string;
    codec: string;
}

export function VideoConverter(): React.JSX.Element {
    const [files, setFiles] = useState<File[]>([]);
    const [format, setFormat] = useState<string>('mp4');
    const [results, setResults] = useState<ConvertedFile[]>([]);
    const [progress, setProgress] = useState<{[fileName: string]: number}>({});
    const [isConverting, setIsConverting] = useState(false);
    const [activeConversions, setActiveConversions] = useState<Set<string>>(new Set());
    
    const handleFilesSelected = (newFiles: File[]) => {
        setFiles(newFiles);
    };

    const handleCancelConversion = (fileName: string) => {
        // Remove from progress and active conversions
        setProgress(prev => {
            const newProgress = { ...prev };
            delete newProgress[fileName];
            return newProgress;
        });
        setActiveConversions(prev => {
            const newActive = new Set(prev);
            newActive.delete(fileName);
            return newActive;
        });
        
        // Emit cancel event to backend
        const socket = io('http://localhost:3001');
        socket.emit('cancel-conversion', { fileName });
        socket.disconnect();
        
        toast.error(`Conversion canceled for ${fileName}`);
    };

    // Socket.IO connection for real-time progress updates
    useEffect(() => {
        const socket: Socket = io('http://localhost:3001');

        socket.on('conversion-progress', (data: {
            fileName: string;
            progress: number;
            outputName?: string;
            completed?: boolean;
        }) => {
            // Add to active conversions if not already there
            if (data.progress === 0) {
                setActiveConversions(prev => new Set(prev).add(data.fileName));
            }
            
            setProgress(prev => ({
                ...prev,
                [data.fileName]: data.progress
            }));

            // Clear progress and remove from active conversions when completed
            if (data.completed) {
                setActiveConversions(prev => {
                    const newActive = new Set(prev);
                    newActive.delete(data.fileName);
                    return newActive;
                });
                
                setTimeout(() => {
                    setProgress(prev => {
                        const newProgress = { ...prev };
                        delete newProgress[data.fileName];
                        return newProgress;
                    });
                }, 2000); // Clear after 2 seconds to show completion
            }
        });

        socket.on('conversion-error', (data: {
            fileName: string;
            error: string;
        }) => {
            setProgress(prev => {
                const newProgress = { ...prev };
                delete newProgress[data.fileName];
                return newProgress;
            });
            setActiveConversions(prev => {
                const newActive = new Set(prev);
                newActive.delete(data.fileName);
                return newActive;
            });
            toast.error(`Conversion failed for ${data.fileName}: ${data.error}`);
        });

        return () => {
            socket.disconnect();
        };
    }, []);

    const convertMutation = useMutation({
        mutationFn: async ({
            file,
            format
        }: {
            file: File;
            format: string;
        }) => {
            const formData = new FormData();
            formData.append('file', file);
            formData.append('format', format);

            const response = await fetch('http://localhost:3001/api/convert', {
                method: 'POST',
                body: formData
            });

            if (!response.ok) {
                const errorText = await response.text();
                let errorMessage = 'Conversion failed';
                
                try {
                    const errorData = JSON.parse(errorText);
                    errorMessage = errorData.error || errorData.message || errorMessage;
                } catch {
                    // If JSON parsing fails, try to extract meaningful error from text
                    if (errorText.includes('File too large')) {
                        errorMessage = 'File is too large for upload';
                    } else if (errorText.includes('Unsupported format')) {
                        errorMessage = 'Unsupported video format';
                    } else if (errorText) {
                        errorMessage = errorText;
                    }
                }
                
                throw new Error(errorMessage);
            }

            return response.json() as Promise<ConversionResponse>;
        },
        onSuccess: (data, variables) => {
            setResults(r => [...r, data]);
        },
        onError: (error: Error, variables) => {
            console.error('Conversion failed:', error);
            toast.error(`Failed to convert ${variables.file.name}: ${error.message}`);
        }
    });

    const handleConvert = async () => {
        if (files.length === 0) {
            toast.error('Please select at least one file to convert');
            return;
        }
        
        // Check file sizes (15GB = 15 * 1024 * 1024 * 1024 bytes)
        const maxFileSize = 15 * 1024 * 1024 * 1024; // 15GB
        const oversizedFiles = files.filter(file => file.size > maxFileSize);
        
        if (oversizedFiles.length > 0) {
            const fileNames = oversizedFiles.map(f => f.name).join(', ');
            toast.error(`File(s) too large (max 15GB): ${fileNames}`);
            return;
        }
        
        setIsConverting(true);
        
        // Initialize progress bars immediately for all files
        const initialProgress: {[fileName: string]: number} = {};
        const activeFileNames = new Set<string>();
        files.forEach(file => {
            initialProgress[file.name] = 0;
            activeFileNames.add(file.name);
        });
        setProgress(initialProgress);
        setActiveConversions(activeFileNames);
        
        try {
            for (const file of files) {
                await convertMutation.mutateAsync({ file, format });
            }
            setFiles([]);
        } catch (error) {
            // Individual file errors are already handled in onError callback
            // This catch is for any unexpected errors in the loop
            console.error('Unexpected error during conversion:', error);
            // Clear progress and active conversions on error
            setProgress({});
            setActiveConversions(new Set());
        } finally {
            setIsConverting(false);
        }
    };

    return (
        <>
            <DropZone onFiles={handleFilesSelected} />
            <div className="flex gap-2">
                <Select onValueChange={setFormat} value={format}>
                    <SelectTrigger className="w-40">
                        <SelectValue placeholder="Format" />
                    </SelectTrigger>
                    <SelectContent>
                        <SelectItem value="mp4">MP4</SelectItem>
                        <SelectItem value="webm">WebM</SelectItem>
                        <SelectItem value="avi">AVI</SelectItem>
                        <SelectItem value="mov">MOV</SelectItem>
                        <SelectItem value="mkv">MKV</SelectItem>
                        <SelectItem value="wmv">WMV</SelectItem>
                    </SelectContent>
                </Select>
                <Button
                    onClick={handleConvert}
                    disabled={convertMutation.isPending || files.length === 0 || isConverting}
                >
                    {isConverting ? 'Converting...' : 'Convert'}
                </Button>
            </div>
            {files.length > 0 && (
                <div className="text-sm text-zinc-400">
                    {files.length} file(s) selected
                </div>
            )}
            {/* Progress bars for active conversions */}
            {Object.keys(progress).length > 0 && (
                <div className="space-y-4">
                    <h3 className="text-sm font-medium text-zinc-200">Converting Files:</h3>
                    {Object.entries(progress).map(([fileName, progressValue]) => (
                        <div key={fileName} className="space-y-2">
                            <div className="flex justify-between items-center text-sm">
                                <div className="flex items-center gap-2 flex-1 min-w-0">
                                    <span className="text-zinc-300 truncate">{fileName}</span>
                                    {activeConversions.has(fileName) && progressValue < 100 && (
                                        <button
                                            onClick={() => handleCancelConversion(fileName)}
                                            className="text-zinc-400 hover:text-red-500 hover:bg-red-500/10 rounded-full p-1 transition-colors duration-200 flex-shrink-0"
                                            title={`Cancel conversion of ${fileName}`}
                                        >
                                            <X size={14} />
                                        </button>
                                    )}
                                </div>
                                <span className="text-zinc-400 font-medium">{progressValue}%</span>
                            </div>
                            <Progress 
                                value={progressValue} 
                                className="h-2"
                            />
                        </div>
                    ))}
                </div>
            )}
            {results.length > 0 && (
                <div className="space-y-2">
                    <h3 className="text-sm font-medium">Converted Files:</h3>
                    <ul className="space-y-1">
                        {results.map((result, index) => (
                            <li
                                key={`${result.outputName}-${index}`}
                                className="border p-2 rounded"
                            >
                                <div className="flex flex-col gap-1">
                                    <a
                                        className="underline text-blue-400 hover:text-blue-300"
                                        href={`http://localhost:3001${result.downloadUrl}`}
                                        rel="noreferrer"
                                        target="_blank"
                                    >
                                        {result.name} → {result.outputName}
                                    </a>
                                    <div className="text-xs text-gray-500">
                                        Duration: {Math.round(result.duration)}s
                                        | Size:{' '}
                                        {Math.round(
                                            result.fileSize / 1024 / 1024
                                        )}
                                        MB | Resolution: {result.resolution}
                                    </div>
                                </div>
                            </li>
                        ))}
                    </ul>
                </div>
            )}
        </>
    );
}
