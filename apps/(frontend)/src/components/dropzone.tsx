import * as React from 'react';
import { useDropzone } from 'react-dropzone';
import { cn } from '../lib/utils';

export interface DropzoneProps {
    onFiles: (files: File[]) => void;
}

export function DropZone({ onFiles }: DropzoneProps): React.JSX.Element {
    const { getRootProps, getInputProps, isDragActive } = useDropzone({
        onDrop: accepted => onFiles(accepted),
        multiple: true,
        accept: { 'video/*': [] }
    });

    return (
        <div
            {...getRootProps()}
            className={cn(
                'flex cursor-pointer flex-col items-center justify-center rounded-md border-2 border-dashed border-border p-8 text-sm transition-colors hover:bg-accent/50',
                isDragActive && 'bg-accent border-primary'
            )}
        >
            <input {...getInputProps()} />
            <p className="text-center text-foreground">
                {isDragActive
                    ? 'Drop videos here...'
                    : 'Drop videos here or click to browse'}
            </p>
        </div>
    );
}
