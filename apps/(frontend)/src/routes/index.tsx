import { createFileRoute } from '@tanstack/react-router';
import { VideoConverter } from '../components/video-converter';

export const Route = createFileRoute('/')({
    component: App
});

function App() {
    return (
        <div className="min-h-screen bg-zinc-950 text-white">
            <div className="container mx-auto px-4 py-8">
                <h1 className="text-4xl font-bold text-center mb-8">
                    Video Converter
                </h1>
                <div className="max-w-2xl mx-auto space-y-4">
                    <VideoConverter />
                </div>
            </div>
        </div>
    );
}
