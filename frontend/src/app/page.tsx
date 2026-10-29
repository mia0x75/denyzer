import { Dashboard } from "~/components/dashboard/dashboard";

export default function HomePage() {
  const showLogs = true;

  return (
    <main className="container mx-auto max-w-5xl px-4 py-6 sm:py-8">
      <header className="mb-6 sm:mb-8">
        {/*
          The Jersey 15 metrics are taller than the em box, so a normal
          line-height leaves a lot of empty space around the logo. The tight
          leading keeps the text close to the surrounding margins.
        */}
        <h1 className="font-title text-5xl leading-[0.85] font-bold sm:text-6xl">
          {"> "}Denyzer+
        </h1>
        {/* xxx */}
      </header>

      <Dashboard showLogs={showLogs} />
    </main>
  );
}
